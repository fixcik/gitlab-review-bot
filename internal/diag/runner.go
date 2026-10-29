package diag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fixcik/gitlab-review-bot/internal/claude"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Runner is the core.Runner decorator returned by WrapRunner: Exec and Run
// become timeline steps of the Run in the context, and every Claude
// run is recorded in full (request, transcript, result). For a traced job
// both are spans too ("exec <tool>", "claude <kind>"). Every method of
// core.Runner must be forwarded here.
type Runner struct {
	next core.Runner
}

var _ core.Runner = (*Runner)(nil)

// WrapRunner returns r recorded into the Run found in each call's context.
// With a nil Recorder it returns r unchanged.
func (rec *Recorder) WrapRunner(r core.Runner) core.Runner { return Hooks{Rec: rec}.WrapRunner(r) }

// WrapRunner returns r recorded (unchanged when off).
func (h Hooks) WrapRunner(r core.Runner) core.Runner {
	if h.off() || r == nil {
		return r
	}
	return &Runner{next: r}
}

// procObserver collects the child processes of one runner call.
type procObserver struct {
	mu    sync.Mutex
	procs []claude.ProcessInfo
	ev    func([]byte)
}

func (o *procObserver) Event(line []byte) {
	if o.ev != nil {
		o.ev(line)
	}
}

func (o *procObserver) Process(p claude.ProcessInfo) {
	o.mu.Lock()
	o.procs = append(o.procs, p)
	o.mu.Unlock()
}

func (o *procObserver) last() (claude.ProcessInfo, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.procs) == 0 {
		return claude.ProcessInfo{}, false
	}
	return o.procs[len(o.procs)-1], true
}

func parentOf(p *pending) int {
	if p == nil {
		return 0
	}
	return p.idx + 1
}

// Exec records the command with its exit code and output tails.
func (d *Runner) Exec(ctx context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	sctx, p := startStep(ctx, execSpanName(req.Command), KindExec, "exec: "+strings.Join(req.Command, " "),
		Fields{"argv": req.Command, "env": envNames(req.Env), "timeout": req.Timeout})
	if p == nil {
		return d.next.Exec(ctx, req)
	}
	if len(req.Command) > 0 {
		p.spanAttrs(tracing.String("process.executable.name", filepath.Base(req.Command[0])))
	}
	obs := &procObserver{}
	res, err := d.next.Exec(claude.WithObserver(sctx, obs), req)
	f := Fields{}
	if pi, ok := obs.last(); ok {
		f["process_argv"] = pi.Argv
	}
	if res != nil {
		f["exit_code"] = res.ExitCode
		p.spanAttrs(tracing.Int("process.exit.code", res.ExitCode))
		p.with(func(run *Run, s *TimelineStep) {
			s.Stdout = run.rec.scrub.str(tail(res.Stdout, TailBytes), run.safe)
			s.Stderr = run.rec.scrub.str(tail(res.Stderr, TailBytes), run.safe)
		})
		status := StepOK
		if res.ExitCode != 0 {
			status = StepWarn
		}
		p.end(status, fmt.Sprintf("exit %d", res.ExitCode), f)
		return res, err
	}
	p.done(err, f)
	return res, err
}

func envNames(env []string) []string {
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		out = append(out, name+"=…")
	}
	return out
}

// Run records the Claude session: request, transcript, result.
// The span's context goes to the runner: its traceparent is the parent of
// Claude Code's own spans (tracing.claude_cli).
func (d *Runner) Run(ctx context.Context, req core.RunRequest) (*core.RunResult, error) {
	run := FromContext(ctx)
	sctx, sp := startClaudeSpan(ctx, req)
	if run == nil && sp == nil {
		return d.next.Run(ctx, req)
	}
	obs := &procObserver{}
	var cr *claudeRec
	if run != nil {
		cr = run.startClaude(ctx, req)
		obs.ev = cr.event
	}
	res, err := d.next.Run(claude.WithObserver(sctx, obs), req)
	pi, _ := obs.last()
	if cr != nil {
		cr.finish(res, err, pi)
	}
	endClaudeSpan(sp, res, err, pi)
	return res, err
}

// claudeRec is one Claude run being recorded.
type claudeRec struct {
	run  *Run
	n    int // 1-based
	step *pending

	mu        sync.Mutex
	f         *os.File
	bytes     int64
	events    int
	truncated bool
}

func (run *Run) startClaude(ctx context.Context, req core.RunRequest) *claudeRec {
	purpose := ""
	if a, ok := meter.From(ctx); ok {
		purpose = a.Kind
	}
	name := "claude"
	if purpose != "" {
		name += ": " + purpose
	}
	if req.Model != "" {
		name += " (" + req.Model + ")"
	}
	p := run.begin(KindClaude, name, nil)
	c := &claudeRec{run: run, step: p}
	sc := run.rec.scrub
	run.mu.Lock()
	c.n = len(run.r.Claude) + 1
	cr := ClaudeRun{
		N: c.n, Step: parentOf(p), Purpose: purpose, Start: run.rec.o.Now(), Model: req.Model, Dir: req.Dir,
		NoTools: req.NoTools, SystemPrompt: sc.str(req.SystemPrompt, run.safe), Prompt: sc.str(req.Prompt, run.safe),
		JSONSchema: sc.str(req.JSONSchema, run.safe), AllowedTools: claude.AllowedTools(req),
		Disallowed: claude.DisallowedTools(), Status: StepRunning, Transcript: TranscriptName(c.n),
	}
	if req.Timeout > 0 {
		cr.Timeout = req.Timeout.String()
	}
	if req.MCP != nil {
		// Server and tools only: the per-run token is never recorded.
		cr.MCPURL, cr.MCPTools = sc.str(req.MCP.URL, run.safe), append([]string(nil), req.MCP.Tools...)
	}
	for _, cd := range req.ContextDirs {
		cr.ContextDirs = append(cr.ContextDirs, ContextDirInfo{Name: cd.Name, SHA: cd.SHA, Branch: cd.Branch})
		if len(cd.SHA) >= 7 {
			run.safe = append(run.safe, cd.SHA)
		}
	}
	run.r.Claude = append(run.r.Claude, cr)
	if p != nil && p.idx < len(run.r.Steps) {
		run.r.Steps[p.idx].Claude = c.n
	}
	run.flushLocked(true)
	run.mu.Unlock()
	if !run.writeError {
		f, err := os.OpenFile(filepath.Join(run.dir, TranscriptName(c.n)), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			run.rec.log.Warn("diag: create transcript", "run", run.r.ID, "err", err)
		} else {
			c.f = f
		}
	}
	return c
}

// budgetLeft is what the transcripts of the run may still take.
func (c *claudeRec) budgetLeft() int64 {
	run := c.run
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.rec.o.MaxRunBytes - minJSONBudget - run.tBytes
}

func (c *claudeRec) event(line []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events++
	if c.f == nil {
		return
	}
	isResult := isResultEvent(line)
	if c.truncated && !isResult {
		return
	}
	b := append(c.run.rec.scrub.jsonLine(line, c.run.safeCopy()), '\n')
	if !isResult && int64(len(b)) > c.budgetLeft() {
		c.truncated = true
		marker := []byte(fmt.Sprintf(`{"type":"reviewbot_truncated","reason":"превышен diag.max_run_mb (%d МБ): остальные события до result не сохранены"}`+"\n",
			c.run.rec.o.MaxRunBytes>>20))
		c.write(marker)
		return
	}
	c.write(b)
}

func (c *claudeRec) write(b []byte) {
	n, err := c.f.Write(b)
	if err != nil {
		c.run.rec.log.Warn("diag: write transcript", "err", err)
		_ = c.f.Close()
		c.f = nil
	}
	c.bytes += int64(n)
	c.run.mu.Lock()
	c.run.tBytes += int64(n)
	c.run.mu.Unlock()
}

func isResultEvent(line []byte) bool {
	// Cheap check; the stream capture already parsed it.
	s := string(line)
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Contains(s, `"type":"result"`)
}

func (run *Run) safeCopy() []string {
	run.mu.Lock()
	defer run.mu.Unlock()
	return append([]string(nil), run.safe...)
}

func (c *claudeRec) finish(res *core.RunResult, err error, pi claude.ProcessInfo) {
	c.mu.Lock()
	if c.f != nil {
		_ = c.f.Close()
		c.f = nil
	}
	events, bytes, truncated := c.events, c.bytes, c.truncated
	c.mu.Unlock()

	run := c.run
	sc := run.rec.scrub
	metrics := meter.RunMetrics(res, err)
	run.mu.Lock()
	cr := &run.r.Claude[c.n-1]
	cr.DurationMS = run.rec.o.Now().Sub(cr.Start).Milliseconds()
	cr.Events, cr.TranscriptBytes, cr.TranscriptTruncated = events, bytes, truncated
	if pi.Argv != nil {
		cr.Argv = run.scrubAll(pi.Argv)
		code := pi.ExitCode
		cr.ExitCode = &code
		cr.Stderr = sc.str(pi.Stderr, run.safe)
	}
	if res != nil {
		cr.Output = sc.str(res.Output, run.safe)
	}
	if metrics != nil {
		cr.Usage, cr.CostUSD, cr.NumTurns, cr.CLIMS = tokensOf(metrics.Usage), metrics.CostUSD, metrics.NumTurns, metrics.Duration.Milliseconds()
		if len(metrics.Models) > 0 {
			cr.Models = map[string]ModelTokens{}
			for k, m := range metrics.Models {
				cr.Models[k] = ModelTokens{Tokens: tokensOf(m.TokenUsage), CostUSD: m.CostUSD}
			}
		}
	}
	if err != nil {
		cr.Status, cr.Error, cr.ErrorClass = StepError, sc.str(err.Error(), run.safe), errorClass(err)
	} else {
		cr.Status = StepOK
	}
	usage, cost, turns := cr.Usage, cr.CostUSD, cr.NumTurns
	run.mu.Unlock()

	f := Fields{"tokens": usage.Total(), "cost_usd": cost, "turns": turns, "events": events}
	if err != nil {
		f["error_class"] = errorClass(err)
	}
	c.step.done(err, f)
}

// errorClass is meter.ErrorClass plus "postponed" for RetryLater.
func errorClass(err error) string {
	if _, ok := core.AsRetryLater(err); ok {
		return "postponed"
	}
	return meter.ErrorClass(err)
}
