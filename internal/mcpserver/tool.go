package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Tool is one tool of a session. Handler gets the raw "arguments" object
// and returns the text content of the result; an error becomes a result
// with isError=true and the error text (the model sees it and can react).
type Tool struct {
	Name        string
	Description string
	// InputSchema is the JSON Schema of the arguments (an object schema).
	InputSchema json.RawMessage
	// ReadOnly marks tools that change nothing (annotations.readOnlyHint).
	ReadOnly bool
	Handler  func(ctx context.Context, args json.RawMessage) (string, error)
}

// toolNameRe: names Claude Code accepts in mcp__<server>__<tool>.
var toolNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// CallInfo describes one finished tools/call (no arguments beyond a short
// preview, never the token).
type CallInfo struct {
	Tool        string
	Status      string
	Duration    time.Duration
	Bytes       int // response text size
	ArgsPreview string
	ProjectID   int64
	MRIID       int64
	RunID       string
}

// Result is the outcome of one tool call.
type Result struct {
	Text    string
	IsError bool
	Status  string
}

// Call runs one tool as tools/call would (HTTP requests end up here too).
// ctx bounds the call in addition to the session's Ctx and CallTimeout.
func (ss *Session) Call(ctx context.Context, name string, args json.RawMessage) (res Result, known bool) {
	start := ss.srv.o.Now()
	defer func() {
		if !known {
			return
		}
		ss.observe(name, res, args, ss.srv.o.Now().Sub(start))
	}()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return Result{Text: "the review run has ended", IsError: true, Status: StatusError}, true
	}
	t, ok := ss.tools[name]
	if !ok {
		return Result{Status: StatusUnknown}, false
	}
	if ss.calls >= ss.srv.o.MaxCalls {
		ss.calls++
		return Result{Text: fmt.Sprintf("tool call limit of this review (%d) reached: stop calling tools, finish the review and put any remaining findings into the final JSON answer", ss.srv.o.MaxCalls),
			IsError: true, Status: StatusLimit}, true
	}
	ss.calls++
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	cctx, cancel := context.WithTimeout(ss.spec.Ctx, ss.srv.o.CallTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	// A traced job: the call is a server span of the job's trace (the
	// session's Ctx carries the job span). Tool name, status and size
	// only: never the arguments or the answer.
	cctx, sp := tracing.Start(cctx, "mcp tools/call "+name, tracing.WithKind(tracing.KindServer),
		tracing.WithAttrs(tracing.String("mcp.method.name", "tools/call"), tracing.String("gen_ai.tool.name", name),
			tracing.Int("reviewbot.mcp.call", ss.calls), tracing.Int("reviewbot.mcp.args_bytes", len(args))))
	text, err := safeCall(cctx, t, args)
	if err != nil {
		text, res.IsError, res.Status = err.Error(), true, StatusError
	} else {
		res.Status = StatusOK
	}
	res.Text = ss.srv.capText(text)
	sp.SetAttr(tracing.String("reviewbot.mcp.status", res.Status), tracing.Int("reviewbot.mcp.result_bytes", len(res.Text)))
	if err != nil {
		sp.SetAttr(tracing.String("error.type", "tool_error"))
		sp.SetStatus(tracing.StatusError, "tool error")
	}
	sp.End()
	return res, true
}

func safeCall(ctx context.Context, t Tool, args json.RawMessage) (text string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("internal error in tool %s", t.Name)
		}
	}()
	return t.Handler(ctx, args)
}

// capText cuts text to MaxResponseBytes (on a rune boundary) with a note.
func (s *Server) capText(text string) string {
	limit := s.o.MaxResponseBytes
	if len(text) <= limit {
		return text
	}
	note := fmt.Sprintf("\n…[truncated: the answer exceeded %d KB; narrow the request, e.g. pass a path]", limit>>10)
	cut := max(limit-len(note), 0)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + note
}

func (ss *Session) observe(name string, res Result, args json.RawMessage, d time.Duration) {
	c := CallInfo{Tool: name, Status: res.Status, Duration: d, Bytes: len(res.Text), ArgsPreview: preview(args, 120),
		ProjectID: ss.spec.ProjectID, MRIID: ss.spec.MRIID, RunID: ss.spec.RunID}
	ss.srv.o.Logger.Info("mcp: tool call", "tool", name, "status", c.Status, "duration", d.Round(time.Millisecond),
		"bytes", c.Bytes, "project_id", c.ProjectID, "mr_iid", c.MRIID, "run", c.RunID)
	if f := ss.srv.o.OnCall; f != nil {
		f(ss.spec.Ctx, c)
	}
}

// preview is a one-line, size-capped view of the arguments for logs.
func preview(args json.RawMessage, n int) string {
	s := strings.Join(strings.Fields(string(args)), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
