package claude

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// A stream-json session shaped like Claude Code 2.1.x output with
// --output-format stream-json --verbose.
const streamSession = `{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus","tools":["Read","Grep","StructuredOutput"]}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me look."},{"type":"tool_use","id":"tu1","name":"Read","input":{"file_path":"/work/main.go"}}]},"session_id":"s1"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"package main\n"}]},"session_id":"s1"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1735689600}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu2","name":"StructuredOutput","input":{"summary":"ok","findings":[]}}]},"session_id":"s1"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu2","content":"Structured output provided successfully"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"summary":"ok","findings":[]},"total_cost_usd":0.42,"num_turns":3,"duration_ms":2500,"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":40},"modelUsage":{"claude-opus":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":300,"cacheCreationInputTokens":40,"costUSD":0.42}}}
`

type recObserver struct {
	mu     sync.Mutex
	events []string
	procs  []ProcessInfo
}

func (o *recObserver) Event(l []byte) {
	o.mu.Lock()
	o.events = append(o.events, string(l))
	o.mu.Unlock()
}
func (o *recObserver) Process(p ProcessInfo) {
	o.mu.Lock()
	o.procs = append(o.procs, p)
	o.mu.Unlock()
}

// feed writes s to a capture in odd-sized chunks (lines split across writes).
func feed(sc *streamCapture, s string, chunk int) {
	for len(s) > 0 {
		n := min(chunk, len(s))
		_, _ = sc.Write([]byte(s[:n]))
		s = s[n:]
	}
}

func TestStreamResultSameAsJSON(t *testing.T) {
	obs := &recObserver{}
	sc := newStreamCapture(WithObserver(context.Background(), obs))
	feed(sc, streamSession, 7)
	res, err := parseOutput(processOutput{Stdout: sc.Bytes()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != `{"findings":[],"summary":"ok"}` || res.CostUSD != 0.42 || res.NumTurns != 3 || res.Duration != 2500*time.Millisecond {
		t.Fatalf("res = %+v", res)
	}
	if res.Usage != (core.TokenUsage{Input: 10, Output: 20, CacheRead: 300, CacheWrite: 40}) || res.Models["claude-opus"].CostUSD != 0.42 {
		t.Fatalf("usage = %+v %+v", res.Usage, res.Models)
	}
	// The single-object json form of the same result parses identically.
	lines := strings.Split(strings.TrimSpace(streamSession), "\n")
	old, err := parseOutput(processOutput{Stdout: []byte(lines[len(lines)-1])}, true)
	if err != nil || old.Output != res.Output || old.Usage != res.Usage || old.CostUSD != res.CostUSD {
		t.Fatalf("json form differs: %+v %v", old, err)
	}
	if len(obs.events) != 7 || !strings.Contains(obs.events[0], `"subtype":"init"`) || !strings.Contains(obs.events[1], `"tool_use"`) ||
		!strings.Contains(obs.events[2], `"tool_result"`) || !strings.Contains(obs.events[6], `"type":"result"`) {
		t.Fatalf("events: %d %q", len(obs.events), obs.events)
	}
}

func TestStreamTextResult(t *testing.T) {
	sc := newStreamCapture(context.Background())
	feed(sc, `{"type":"system","subtype":"init"}`+"\n"+`{"type":"result","subtype":"success","result":"Hello","num_turns":1}`, 1000)
	res, err := parseOutput(processOutput{Stdout: sc.Bytes()}, false)
	if err != nil || res.Output != "Hello" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestStreamToleratesNoiseAndInvalidLines(t *testing.T) {
	obs := &recObserver{}
	sc := newStreamCapture(WithObserver(context.Background(), obs))
	feed(sc, "warning: something\n{not json\n{\"type\":\"assistant\",\"message\":{\"content\":[]}}\n{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"x\"}\n{\"type\":\"assistant\",\"mess", 5)
	res, err := parseOutput(processOutput{Stdout: sc.Bytes()}, false)
	if err != nil || res.Output != "x" {
		t.Fatalf("%+v %v", res, err)
	}
	// The truncated trailing line is not an event; noise is not either.
	if len(obs.events) != 2 {
		t.Fatalf("events: %q", obs.events)
	}
}

func TestStreamNoResult(t *testing.T) {
	// A crash before the result event: the non-event output is what the
	// error reports.
	sc := newStreamCapture(context.Background())
	feed(sc, `{"type":"system","subtype":"init"}`+"\nError: boom\n", 3)
	_, err := parseOutput(processOutput{Stdout: sc.Bytes(), ExitCode: 1, Err: errors.New("exit status 1"), Stderr: []byte("fatal")}, false)
	if err == nil || !strings.Contains(err.Error(), "exited with code 1") {
		t.Fatalf("err = %v", err)
	}
	// Truncated output with nothing parseable and exit 0.
	sc = newStreamCapture(context.Background())
	feed(sc, `{"type":"assistant","message":{"content":[`, 4)
	if _, err := parseOutput(processOutput{Stdout: sc.Bytes()}, false); err == nil {
		t.Fatal("want error")
	}
}

func TestStreamErrorResultKeepsUsage(t *testing.T) {
	sc := newStreamCapture(context.Background())
	feed(sc, `{"type":"system","subtype":"init"}
{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":50,"total_cost_usd":1.5,"usage":{"input_tokens":5,"output_tokens":6}}
`, 9)
	_, err := parseOutput(processOutput{Stdout: sc.Bytes(), ExitCode: 1}, true)
	var fr *core.FailedRun
	if !errors.As(err, &fr) || fr.Partial.CostUSD != 1.5 || fr.Partial.Usage.Total() != 11 || !strings.Contains(err.Error(), "error_max_turns") {
		t.Fatalf("err = %#v", err)
	}
}

func TestStreamRateLimits(t *testing.T) {
	// 1. Usage limit in the result text.
	sc := newStreamCapture(context.Background())
	feed(sc, `{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached|1735689600"}`+"\n", 11)
	_, err := parseOutput(processOutput{Stdout: sc.Bytes(), ExitCode: 1}, true)
	var rl *core.ErrClaudeRateLimited
	if !errors.As(sc.refineError(err), &rl) || rl.ResetAt.Unix() != 1735689600 {
		t.Fatalf("result text: %v", err)
	}
	// 2. Only a rejected rate_limit_event, and a crash without result.
	sc = newStreamCapture(context.Background())
	feed(sc, `{"type":"system","subtype":"init"}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1735689600,"rateLimitType":"five_hour"}}
`, 13)
	_, err = parseOutput(processOutput{Stdout: sc.Bytes(), ExitCode: 1, Err: errors.New("exit 1")}, true)
	rl = nil
	if !errors.As(sc.refineError(err), &rl) || rl.ResetAt.Unix() != 1735689600 {
		t.Fatalf("event only: %v", err)
	}
	// 3. A rejected event plus a generic error result -> rate limited, usage kept.
	sc = newStreamCapture(context.Background())
	feed(sc, `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1735689600}}
{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","total_cost_usd":0.1,"usage":{"input_tokens":1}}
`, 17)
	_, err = parseOutput(processOutput{Stdout: sc.Bytes(), ExitCode: 1}, true)
	err = sc.refineError(err)
	var fr *core.FailedRun
	rl = nil
	if !errors.As(err, &rl) || !errors.As(err, &fr) || fr.Partial.CostUSD != 0.1 {
		t.Fatalf("event + error result: %#v", err)
	}
	// 4. An allowed rate_limit_event does not turn a failure into a limit.
	sc = newStreamCapture(context.Background())
	feed(sc, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1735689600}}
{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Invalid API key"}
`, 17)
	_, err = parseOutput(processOutput{Stdout: sc.Bytes(), ExitCode: 1}, true)
	if errors.As(sc.refineError(err), &rl) && rl != nil && strings.Contains(err.Error(), "usage limit") {
		t.Fatalf("false limit: %v", err)
	}
	// 5. API error on an assistant message (no result) is reported.
	sc = newStreamCapture(context.Background())
	feed(sc, `{"type":"assistant","error":"rate_limit","message":{"content":[{"type":"text","text":"API Error: 429 rate_limit_error"}]}}`+"\n", 5)
	_, err = parseOutput(processOutput{Stdout: sc.Bytes(), ExitCode: 1, Err: errors.New("exit 1")}, true)
	rl = nil
	if !errors.As(err, &rl) {
		t.Fatalf("assistant api error: %v", err)
	}
}

func TestStreamOverlongLineDropped(t *testing.T) {
	obs := &recObserver{}
	sc := newStreamCapture(WithObserver(context.Background(), obs))
	sc.Write([]byte(`{"type":"user","x":"`))
	big := strings.Repeat("a", 1<<20)
	for i := 0; i < maxStreamLine/len(big)+1; i++ {
		sc.Write([]byte(big))
	}
	sc.Write([]byte("\"}\n" + `{"type":"result","subtype":"success","result":"ok"}` + "\n"))
	res, err := parseOutput(processOutput{Stdout: sc.Bytes()}, false)
	if err != nil || res.Output != "ok" {
		t.Fatalf("%v %v", res, err)
	}
	if len(obs.events) != 2 || !strings.Contains(obs.events[0], "reviewbot_dropped") {
		t.Fatalf("events: %d", len(obs.events))
	}
}

func TestSanitizeArgv(t *testing.T) {
	got := sanitizeArgv([]string{"claude", "-p", "--append-system-prompt", "long system", "--json-schema", `{"a":1}`, "--model", "opus"})
	want := "claude -p --append-system-prompt <system prompt: 11 bytes> --json-schema <json schema: 7 bytes> --model opus"
	if strings.Join(got, " ") != want {
		t.Fatalf("%q", got)
	}
}

func TestRunStreamsToObserver(t *testing.T) {
	bin := t.TempDir()
	docker := writeScript(t, bin, "docker", `
cat >/dev/null
cat <<'EOF'
`+streamSession+`EOF
echo "some warning" >&2
`)
	o := dockerOpts()
	o.DockerBin = docker
	r, err := newRunner(o, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "CLAUDE_CODE_OAUTH_TOKEN=sk-oauth-secret"}
	})
	if err != nil {
		t.Fatal(err)
	}
	obs := &recObserver{}
	res, err := r.Run(WithObserver(context.Background(), obs), core.RunRequest{Dir: t.TempDir(), Prompt: "p", SystemPrompt: "zqx", JSONSchema: `{"type":"object"}`})
	if err != nil || res.Output != `{"findings":[],"summary":"ok"}` {
		t.Fatalf("%+v %v", res, err)
	}
	if len(obs.events) != 7 || len(obs.procs) != 1 {
		t.Fatalf("events %d procs %d", len(obs.events), len(obs.procs))
	}
	p := obs.procs[0]
	argv := strings.Join(p.Argv, " ")
	if !strings.Contains(argv, "--output-format stream-json --verbose") || !strings.Contains(argv, "<system prompt: 3 bytes>") ||
		strings.Contains(argv, "zqx") || strings.Contains(argv, "sk-oauth-secret") || !strings.Contains(p.Stderr, "some warning") || p.ExitCode != 0 {
		t.Fatalf("proc: %+v", p)
	}
	// Exec reports its process too.
	obs = &recObserver{}
	if _, err := r.Exec(WithObserver(context.Background(), obs), core.ExecRequest{Dir: t.TempDir(), Command: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if len(obs.procs) != 1 {
		t.Fatalf("exec procs: %d", len(obs.procs))
	}
}

func TestStreamRecordsReadFiles(t *testing.T) {
	sc := newStreamCapture(context.Background())
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"смотрю"},{"type":"tool_use","name":"Read","input":{"file_path":"/skills/go-style/SKILL.md"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"x"}},{"type":"tool_use","name":"Read","input":{"file_path":"/work/main.go"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/skills/go-style/SKILL.md"}}]}}`,
		`{"type":"result","subtype":"success","result":"ok"}`,
	}
	for _, l := range lines {
		sc.Write([]byte(l + "\n"))
	}
	if got := strings.Join(sc.ReadFiles(), ","); got != "/skills/go-style/SKILL.md,/work/main.go" {
		t.Errorf("read files %q", got)
	}
}

// The init event lists the MCP servers with their connection status (the
// first init event counts; servers not listed report "").
func TestStreamMCPStatus(t *testing.T) {
	sc := newStreamCapture(context.Background())
	feed(sc, `{"type":"system","subtype":"init","session_id":"s1","tools":["Read","mcp__reviewbot__submit_finding"],"mcp_servers":[{"name":"reviewbot","status":"connected"},{"name":"other","status":"Failed"}]}
{"type":"system","subtype":"init","mcp_servers":[{"name":"reviewbot","status":"failed"}]}
{"type":"result","subtype":"success","result":"ok"}
`, 5)
	if got := sc.MCPStatus("reviewbot"); got != "connected" {
		t.Errorf("reviewbot: %q", got)
	}
	if got := sc.MCPStatus("other"); got != "failed" {
		t.Errorf("other: %q", got)
	}
	if got := sc.MCPStatus("missing"); got != "" {
		t.Errorf("missing: %q", got)
	}
	none := newStreamCapture(context.Background())
	feed(none, `{"type":"system","subtype":"init","session_id":"s1"}`+"\n", 100)
	if got := none.MCPStatus("reviewbot"); got != "" {
		t.Errorf("no mcp_servers: %q", got)
	}
}

// A deep-review Task call that comes back as an error is recorded (the run
// itself goes on); a failed code-search call or a successful deep-review
// call is not.
func TestStreamDeepReviewFailure(t *testing.T) {
	const s = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Task","input":{"subagent_type":"code-search","prompt":"x"}},{"type":"tool_use","id":"t2","name":"Agent","input":{"subagent_type":"deep-review","prompt":"y"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"search failed"}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"API Error: model claude-opus is not available on your plan"}]}]}}
{"type":"result","subtype":"success","is_error":false,"result":"ok"}
`
	sc := newStreamCapture(context.Background())
	_, _ = sc.Write([]byte(s))
	if got := sc.DeepReviewError(); !strings.Contains(got, "not available on your plan") {
		t.Errorf("deep-review error = %q", got)
	}
	ok := newStreamCapture(context.Background())
	_, _ = ok.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Task","input":{"subagent_type":"deep-review"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"nothing found"}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"zz","is_error":true,"content":"other"}]}}
`))
	if got := ok.DeepReviewError(); got != "" {
		t.Errorf("successful call reported: %q", got)
	}
	long := newStreamCapture(context.Background())
	_, _ = long.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Task","input":{"subagent_type":"deep-review"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t3","is_error":true,"content":"` + strings.Repeat("e", 900) + `"}]}}
`))
	if got := long.DeepReviewError(); len(got) > 400 || got == "" {
		t.Errorf("error text not capped: %d", len(got))
	}
	empty := newStreamCapture(context.Background())
	_, _ = empty.Write([]byte(`{"type":"user"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t4","name":"Task","input":{"subagent_type":"deep-review"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t4","is_error":true,"content":""}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t4","is_error":true,"content":"later"}]}}
`))
	if got := empty.DeepReviewError(); got != "error" {
		t.Errorf("empty error text = %q", got)
	}
}
