package claude

// stream-json output and run observation.
//
// Claude runs use `--output-format stream-json --verbose`: the CLI prints one
// JSON event per line (system init, assistant messages with tool_use blocks,
// user messages with tool_result blocks, rate_limit_event, ...) and finally a
// `result` event carrying the same fields as the single object printed by
// `--output-format json` (result, structured_output, usage, modelUsage,
// total_cost_usd, num_turns, ...). With -p, stream-json requires --verbose
// (verified against Claude Code 2.1.310: "When using --print,
// --output-format=stream-json requires --verbose").
//
// streamCapture splits stdout into lines as they arrive, hands every event
// line to an Observer (the diagnostics recorder) and keeps only what
// parseOutput needs: the last `result` line, or, when there is none (a crash
// or a non-JSON failure), the non-event output plus synthesized error lines,
// so the existing parsing and rate-limit detection work unchanged.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Observer receives what a runner call did. It is optional and carried in
// the context (WithObserver); the runner never fails because of it.
type Observer interface {
	// Event receives one raw stream-json event line of a Claude run (no
	// trailing newline). The slice must not be retained.
	Event(line []byte)
	// Process is called once per finished child process (claude, each setup
	// command, each exec).
	Process(p ProcessInfo)
}

// ProcessInfo describes one finished child process.
type ProcessInfo struct {
	// Argv is the command line with prompt-sized values (system prompt,
	// JSON schema) replaced by placeholders. Secrets never appear in argv
	// (credentials are passed by name or on a file descriptor).
	Argv     []string
	ExitCode int
	Duration time.Duration
	// Stdout and Stderr are the last ProcessTailBytes bytes.
	Stdout, Stderr string
	Err            string
}

// ProcessTailBytes caps ProcessInfo.Stdout/Stderr.
const ProcessTailBytes = 8 << 10

type observerKey struct{}

// WithObserver returns ctx carrying o for runner calls made with it.
func WithObserver(ctx context.Context, o Observer) context.Context {
	return context.WithValue(ctx, observerKey{}, o)
}

func observerFrom(ctx context.Context) Observer {
	o, _ := ctx.Value(observerKey{}).(Observer)
	return o
}

// outputSink collects a child's stdout.
type outputSink interface {
	Write(p []byte) (int, error)
	Bytes() []byte
}

const (
	maxStreamLine = maxCapture // longest event line kept (bigger ones are dropped)
	maxStreamRaw  = 1 << 20    // non-event output kept for error reporting
)

// streamCapture is the stdout sink of a stream-json Claude run.
type streamCapture struct {
	obs      Observer
	partial  []byte
	overlong bool // the current partial line exceeded maxStreamLine

	result   []byte
	raw      bytes.Buffer
	errLines []string
	rejected *rateLimitInfo
	dropped  int
	reads    []string
	readSeen map[string]bool
	// mcp: MCP server statuses of the first init event (name -> status;
	// nil = no init event listed any).
	mcp map[string]string
	// deepIDs: tool_use ids of the Task calls of the deep-review subagent;
	// deepErr: the error text of the first one that failed.
	deepIDs map[string]bool
	deepErr string
}

type rateLimitInfo struct {
	Status        string `json:"status"`
	ResetsAt      int64  `json:"resetsAt"`
	RateLimitType string `json:"rateLimitType"`
}

// streamEvent is the part of an event the capture looks at.
type streamEvent struct {
	Type          string          `json:"type"`
	Error         json.RawMessage `json:"error"`
	RateLimitInfo *rateLimitInfo  `json:"rate_limit_info"`
	Subtype       string          `json:"subtype"`
	// MCPServers (system/init): the MCP servers of the session and their
	// connection status ("connected", "failed", "needs-auth", "pending").
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	Message *struct {
		Content []struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Name  string `json:"name"` // tool_use
			ID    string `json:"id"`   // tool_use
			Input *struct {
				FilePath     string `json:"file_path"`     // Read
				SubagentType string `json:"subagent_type"` // Task / Agent
			} `json:"input"` // tool_use
			ToolUseID string          `json:"tool_use_id"` // tool_result
			IsError   bool            `json:"is_error"`    // tool_result
			Content   json.RawMessage `json:"content"`     // tool_result: string or blocks
		} `json:"content"`
	} `json:"message"`
}

func newStreamCapture(ctx context.Context) *streamCapture {
	return &streamCapture{obs: observerFrom(ctx)}
}

func (s *streamCapture) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendPartial(p)
			break
		}
		s.appendPartial(p[:i])
		s.endLine()
		p = p[i+1:]
	}
	return n, nil
}

func (s *streamCapture) appendPartial(b []byte) {
	if s.overlong {
		return
	}
	if len(s.partial)+len(b) > maxStreamLine {
		s.overlong, s.partial = true, nil
		return
	}
	s.partial = append(s.partial, b...)
}

func (s *streamCapture) endLine() {
	line := s.partial
	s.partial = s.partial[:0]
	if s.overlong {
		s.overlong = false
		s.dropped++
		if s.obs != nil {
			s.obs.Event([]byte(fmt.Sprintf(`{"type":"reviewbot_dropped","reason":"event line longer than %d bytes"}`, maxStreamLine)))
		}
		return
	}
	s.line(bytes.TrimSpace(line))
}

// line handles one complete stdout line.
func (s *streamCapture) line(l []byte) {
	if len(l) == 0 {
		return
	}
	var ev streamEvent
	if l[0] != '{' || json.Unmarshal(l, &ev) != nil || ev.Type == "" {
		s.addRaw(l)
		return
	}
	if s.obs != nil {
		s.obs.Event(l)
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" && s.mcp == nil && len(ev.MCPServers) > 0 {
			s.mcp = map[string]string{}
			for _, m := range ev.MCPServers {
				s.mcp[m.Name] = strings.ToLower(strings.TrimSpace(m.Status))
			}
		}
	case "result":
		s.result = append(s.result[:0], l...)
	case "rate_limit_event":
		if ev.RateLimitInfo != nil && ev.RateLimitInfo.Status == "rejected" {
			ri := *ev.RateLimitInfo
			s.rejected = &ri
		}
	case "assistant":
		s.assistant(ev)
	case "user":
		s.toolResults(ev)
	}
}

// assistant records the files an assistant message reads and, for an API
// error message, a synthesized error line.
func (s *streamCapture) assistant(ev streamEvent) {
	var text string
	if ev.Message != nil {
		for _, c := range ev.Message.Content {
			switch {
			case c.Type == "tool_use" && c.Name == "Read" && c.Input != nil:
				s.addRead(c.Input.FilePath)
			case c.Type == "tool_use" && (c.Name == "Task" || c.Name == "Agent") && c.Input != nil && c.Input.SubagentType == DeepReviewAgentName:
				if s.deepIDs == nil {
					s.deepIDs = map[string]bool{}
				}
				s.deepIDs[c.ID] = true
			case c.Type == "text":
				text += c.Text
			}
		}
	}
	var errName string
	if len(ev.Error) > 0 && json.Unmarshal(ev.Error, &errName) == nil && errName != "" {
		s.errLines = append(s.errLines, "API error ("+errName+"): "+tail(strings.TrimSpace(text), 500))
	}
}

// toolResults records the first failed deep-review Task call of a user
// message (the run itself goes on without the second opinion).
func (s *streamCapture) toolResults(ev streamEvent) {
	if ev.Message == nil || s.deepErr != "" {
		return
	}
	for _, c := range ev.Message.Content {
		if c.Type == "tool_result" && c.IsError && s.deepIDs[c.ToolUseID] {
			s.deepErr = tail(strings.TrimSpace(resultText(c.Content)), 300)
			if s.deepErr == "" {
				s.deepErr = "error"
			}
			return
		}
	}
}

// resultText is the text of a tool_result content: a string or text blocks.
func resultText(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var b strings.Builder
	for _, x := range blocks {
		b.WriteString(x.Text)
	}
	return b.String()
}

// DeepReviewError is the error text of a failed deep-review call ("" = none
// failed).
func (s *streamCapture) DeepReviewError() string { return s.deepErr }

// maxReadFiles caps RunResult.ReadFiles.
const maxReadFiles = 500

// addRead records a file path Claude opened with Read (deduplicated).
func (s *streamCapture) addRead(p string) {
	p = strings.TrimSpace(p)
	if p == "" || len(s.reads) >= maxReadFiles {
		return
	}
	if s.readSeen == nil {
		s.readSeen = map[string]bool{}
	}
	if !s.readSeen[p] {
		s.readSeen[p] = true
		s.reads = append(s.reads, p)
	}
}

// MCPStatus is the status the init event reported for the MCP server name
// ("" = not reported: no init event, or the server is not listed).
func (s *streamCapture) MCPStatus(name string) string { return s.mcp[name] }

// ReadFiles returns the paths recorded by addRead.
func (s *streamCapture) ReadFiles() []string { return append([]string(nil), s.reads...) }

func (s *streamCapture) addRaw(l []byte) {
	if s.raw.Len()+len(l)+1 > maxStreamRaw {
		return
	}
	s.raw.Write(l)
	s.raw.WriteByte('\n')
}

// flush processes a trailing line without a newline.
func (s *streamCapture) flush() {
	if len(s.partial) > 0 || s.overlong {
		s.endLine()
	}
}

// Bytes is what parseOutput sees: the last result event, else the non-event
// output plus synthesized error lines (API errors, a rejected rate limit).
func (s *streamCapture) Bytes() []byte {
	s.flush()
	if len(s.result) > 0 {
		return s.result
	}
	var b bytes.Buffer
	b.Write(s.raw.Bytes())
	for _, l := range s.errLines {
		b.WriteString(l + "\n")
	}
	if s.rejected != nil {
		b.WriteString(s.rejected.message() + "\n")
	}
	return b.Bytes()
}

func (ri *rateLimitInfo) message() string {
	m := "Claude usage limit reached"
	if ri.RateLimitType != "" {
		m += " (" + ri.RateLimitType + ")"
	}
	if ri.ResetsAt > 0 {
		m += fmt.Sprintf("|%d", ri.ResetsAt)
	}
	return m
}

// refineError turns a failed run into *core.ErrClaudeRateLimited when the
// stream reported a rejected rate limit but the result text did not say so.
func (s *streamCapture) refineError(err error) error {
	if err == nil || s.rejected == nil {
		return err
	}
	var rl *core.ErrClaudeRateLimited
	if errors.As(err, &rl) {
		return err
	}
	nrl := detectRateLimit(s.rejected.message())
	var fr *core.FailedRun
	if errors.As(err, &fr) {
		return &core.FailedRun{Err: nrl, Partial: fr.Partial}
	}
	return nrl
}

// observeProcess reports a finished child process to the observer in ctx.
func observeProcess(ctx context.Context, cmd *exec.Cmd, p processOutput, took time.Duration) {
	o := observerFrom(ctx)
	if o == nil {
		return
	}
	info := ProcessInfo{
		Argv:     sanitizeArgv(cmd.Args),
		ExitCode: p.ExitCode,
		Duration: took,
		Stdout:   tailBytes8K(p.Stdout),
		Stderr:   tailBytes8K(p.Stderr),
	}
	if p.Err != nil {
		info.Err = p.Err.Error()
	}
	o.Process(info)
}

func tailBytes8K(b []byte) string {
	if len(b) > ProcessTailBytes {
		b = b[len(b)-ProcessTailBytes:]
	}
	return string(b)
}

// promptFlags carry prompt-sized values that are recorded separately.
var promptFlags = map[string]string{
	"--append-system-prompt": "system prompt",
	"--json-schema":          "json schema",
	// A path today; redacted anyway so a JSON value (with the run's MCP
	// token) could never reach /runs.
	"--mcp-config": "mcp config",
}

// sanitizeArgv copies argv with the system prompt and JSON schema replaced
// by "<system prompt: N bytes>" placeholders.
func sanitizeArgv(argv []string) []string {
	out := make([]string, len(argv))
	copy(out, argv)
	for i := 0; i+1 < len(out); i++ {
		if what, ok := promptFlags[out[i]]; ok {
			out[i+1] = fmt.Sprintf("<%s: %d bytes>", what, len(out[i+1]))
			i++
		}
	}
	return out
}

// DisallowedTools returns the deny rules every docker-mode run gets (for
// diagnostics): the denied tools and the Read rules of dockerReadScope.
func DisallowedTools() []string {
	return append(append([]string(nil), disallowedTools...), dockerReadScope(readOnlyDirs{}).denyRules()...)
}
