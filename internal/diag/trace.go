package diag

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/claude"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Tracing bridge: the decorators of this package (GitLab, Limiter, Repos,
// Checkout, Runner) and Step already mark every interesting moment of a
// job; startStep turns each of them into a span of the job's trace as well
// as a timeline step. The two sinks are independent: diagnostics without
// tracing record steps only; tracing without diagnostics (Hooks{Trace:
// true}, nil Recorder) creates spans only. Log lines and Note stay
// timeline-only.
//
// What a span gets from the step fields is deliberately narrow
// (stepAttrs): numbers, booleans and a short list of id-like strings. File
// lists, argv, branch names, user names, previews of arguments and every
// free text stay out; the tracer scrubs and caps what remains.

// Job span attributes set from the diagnostics outcome.
const (
	AttrJobResult = "reviewbot.job.result" // ok, skipped, postponed, failed, interrupted
	AttrJobReason = "reviewbot.job.reason" // skip/postpone reason (scrubbed, capped)
)

type jobSpanKey struct{}

// jobSpan returns the root span of the job in ctx (nil when not traced).
func jobSpan(ctx context.Context) *tracing.Span {
	sp, _ := ctx.Value(jobSpanKey{}).(*tracing.Span)
	return sp
}

// startStep starts a step of the job in ctx: a timeline step of its Run
// (diagnostics on) and a child span named spanName (job traced). The
// returned ctx carries the span, so the wrapped call's own spans (GitLab
// HTTP requests) nest under it. Both off: (ctx, nil).
func startStep(ctx context.Context, spanName, kind, name string, f Fields) (context.Context, *pending) {
	return startStepKind(ctx, spanName, tracing.KindInternal, kind, name, f)
}

func startStepKind(ctx context.Context, spanName string, sk tracing.Kind, kind, name string, f Fields) (context.Context, *pending) {
	p := FromContext(ctx).begin(kind, name, f)
	sctx, sp := tracing.Start(ctx, spanName, tracing.WithKind(sk),
		tracing.WithAttrs(append([]tracing.Attr{tracing.String("reviewbot.step.kind", kind)}, stepAttrs(f)...)...))
	if sp == nil {
		return ctx, p
	}
	if p == nil {
		p = &pending{}
	}
	p.span = sp
	return sctx, p
}

// spanAttrs sets attributes on the step's span only.
func (p *pending) spanAttrs(attrs ...tracing.Attr) {
	if p != nil {
		p.span.SetAttr(attrs...)
	}
}

// endSpan ends the step's span with the step status.
func (p *pending) endSpan(status string, err error, f Fields) {
	if p == nil || p.span == nil {
		return
	}
	sp := p.span
	sp.SetAttr(stepAttrs(f)...)
	sp.SetAttr(tracing.String("reviewbot.step.status", status))
	if status == StepError {
		if err != nil {
			sp.RecordError(err, errorClass(err))
		} else {
			sp.SetStatus(tracing.StatusError, "")
		}
	}
	sp.End()
}

// spanStringFields are the step fields whose string values may go into a
// span: ids, SHAs, states and short enumerations. Everything else (paths,
// file lists, argv, branch and user names, argument previews, details) is
// left out.
var spanStringFields = map[string]bool{
	"state": true, "head_sha": true, "base_sha": true, "start_sha": true, "commit": true, "from": true, "to": true,
	"marker": true, "kind": true, "reason": true, "error_class": true, "discussion_id": true, "status": true,
}

// stepAttrs converts step fields into span attributes "reviewbot.<key>".
func stepAttrs(f Fields) []tracing.Attr {
	if len(f) == 0 {
		return nil
	}
	var out []tracing.Attr
	for _, k := range slices.Sorted(maps.Keys(f)) {
		name := "reviewbot." + k
		switch v := f[k].(type) {
		case int:
			out = append(out, tracing.Int(name, v))
		case int64:
			out = append(out, tracing.Int64(name, v))
		case int32:
			out = append(out, tracing.Int64(name, int64(v)))
		case float64:
			out = append(out, tracing.Float64(name, v))
		case bool:
			out = append(out, tracing.Bool(name, v))
		case time.Duration:
			out = append(out, tracing.Int64(name+"_ms", v.Milliseconds()))
		case string:
			if spanStringFields[k] && v != "" {
				out = append(out, tracing.String(name, v))
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Job span
// ---------------------------------------------------------------------------

// TraceJobs wraps the queue handler: every job is the root span of its own
// trace ("review.job", kind consumer), linked to the span of the event
// that produced it (Job.TraceParent: the webhook request or the poll
// round). active maps the job's MR key to its span for log correlation.
// The outcome (reviewbot.job.result) comes from the diagnostics Run when
// one is inside (same classification as /runs), else from the error. Wrap
// it OUTSIDE Recorder.Handler, so the Run sees the span: TraceJobs(tr,
// active, rec.Handler(...)). A nil Tracer returns h unchanged.
func TraceJobs(tr *tracing.Tracer, active *tracing.Active, h core.Handler) core.Handler {
	if tr == nil {
		return h
	}
	return func(ctx context.Context, job core.Job) (err error) {
		opts := []tracing.StartOption{tracing.WithNewRoot(), tracing.WithKind(tracing.KindConsumer), tracing.WithAttrs(jobAttrs(job)...)}
		if sc, perr := tracing.ParseTraceParent(job.TraceParent); perr == nil {
			opts = append(opts, tracing.WithLinks(tracing.Link{SpanContext: sc,
				Attrs: []tracing.Attr{tracing.String("reviewbot.link", "event")}}))
		}
		ctx, sp := tr.Start(ctx, "review.job", opts...)
		ctx = context.WithValue(ctx, jobSpanKey{}, sp)
		if !job.EnqueuedAt.IsZero() {
			if wait := time.Since(job.EnqueuedAt); wait > 0 {
				sp.SetAttr(tracing.Int64("reviewbot.queue.wait_ms", wait.Milliseconds()))
			}
		}
		unregister := active.Register(job.MRKey(), sp.SpanContext())
		defer func() {
			unregister()
			if p := recover(); p != nil {
				sp.SetAttr(tracing.String(AttrJobResult, StatusFailed))
				sp.RecordError(fmt.Errorf("panic: %v", p), "panic")
				sp.End()
				panic(p)
			}
			endJobSpan(sp, err)
		}()
		return h(ctx, job)
	}
}

func jobAttrs(job core.Job) []tracing.Attr {
	attrs := []tracing.Attr{
		tracing.String("job.kind", string(job.Kind)),
		tracing.Bool("job.manual", job.Kind.Manual()),
		tracing.Int64("gitlab.project.id", job.ProjectID),
		tracing.String("gitlab.project", job.ProjectPath),
		tracing.Int64("gitlab.mr.iid", job.MRIID),
	}
	if job.HeadSHA != "" {
		attrs = append(attrs, tracing.String("vcs.head_sha", job.HeadSHA))
	}
	if job.UserID != 0 {
		attrs = append(attrs, tracing.Int64("reviewbot.user_id", job.UserID))
	}
	if job.NoteID != 0 {
		attrs = append(attrs, tracing.Int64("gitlab.note.id", job.NoteID))
	}
	if job.ForceFull {
		attrs = append(attrs, tracing.Bool("reviewbot.force_full", true))
	}
	if job.Pipeline != nil {
		attrs = append(attrs, tracing.Int64("gitlab.pipeline.id", job.Pipeline.ID), tracing.String("gitlab.pipeline.status", job.Pipeline.Status))
	}
	return attrs
}

// endJobSpan sets the outcome when diagnostics did not, and the status.
func endJobSpan(sp *tracing.Span, err error) {
	var rl *core.RetryLater
	switch {
	case err == nil:
		sp.SetAttrIfAbsent(tracing.String(AttrJobResult, StatusOK))
		sp.SetStatus(tracing.StatusOK, "")
	case errors.As(err, &rl):
		sp.SetAttrIfAbsent(tracing.String(AttrJobResult, StatusPostponed), tracing.String(AttrJobReason, rl.Reason))
		sp.SetAttr(tracing.String("reviewbot.job.retry_at", rl.At.UTC().Format(time.RFC3339)))
	case errors.Is(err, context.Canceled):
		sp.SetAttrIfAbsent(tracing.String(AttrJobResult, StatusInterrupted))
		sp.SetAttr(tracing.String("error.type", "canceled"))
	default:
		sp.SetAttrIfAbsent(tracing.String(AttrJobResult, StatusFailed))
		sp.RecordError(err, errorClass(err))
	}
	sp.End()
}

// ---------------------------------------------------------------------------
// Claude and exec spans
// ---------------------------------------------------------------------------

// startClaudeSpan starts the span of one Claude run (kind client, GenAI
// semantic conventions where they fit). Its ctx goes to the runner, which
// hands its traceparent to the CLI (tracing.claude_cli).
func startClaudeSpan(ctx context.Context, req core.RunRequest) (context.Context, *tracing.Span) {
	purpose := meter.KindOf(ctx)
	attrs := []tracing.Attr{
		tracing.String("gen_ai.system", "anthropic"),
		tracing.String("gen_ai.provider.name", "anthropic"),
		tracing.String("gen_ai.operation.name", "invoke_agent"),
		tracing.String("gen_ai.agent.name", "claude-code"),
		tracing.String("reviewbot.claude.kind", purpose),
		tracing.Bool("reviewbot.claude.no_tools", req.NoTools),
		tracing.Bool("reviewbot.claude.mcp", req.MCP != nil),
		tracing.Int("reviewbot.claude.context_repos", len(req.ContextDirs)),
		tracing.Int("reviewbot.claude.prompt_bytes", len(req.Prompt)+len(req.SystemPrompt)),
	}
	if req.Model != "" {
		attrs = append(attrs, tracing.String("gen_ai.request.model", req.Model))
	}
	if req.Advisor != "" {
		attrs = append(attrs, tracing.String("reviewbot.claude.advisor_model", req.Advisor))
	}
	if req.Timeout > 0 {
		attrs = append(attrs, tracing.Int64("reviewbot.claude.timeout_s", int64(req.Timeout.Seconds())))
	}
	return tracing.Start(ctx, "claude "+purpose, tracing.WithKind(tracing.KindClient), tracing.WithAttrs(attrs...))
}

// endClaudeSpan records usage, cost and the result. Token attributes
// follow the GenAI conventions: input_tokens includes the cached input
// (cache_read + cache_creation are also given separately).
func endClaudeSpan(sp *tracing.Span, res *core.RunResult, err error, pi claude.ProcessInfo) {
	if sp == nil {
		return
	}
	if m := meter.RunMetrics(res, err); m != nil {
		u := m.Usage
		sp.SetAttr(
			tracing.Int64("gen_ai.usage.input_tokens", u.Input+u.CacheRead+u.CacheWrite),
			tracing.Int64("gen_ai.usage.output_tokens", u.Output),
			tracing.Int64("gen_ai.usage.cache_read.input_tokens", u.CacheRead),
			tracing.Int64("gen_ai.usage.cache_creation.input_tokens", u.CacheWrite),
			tracing.Float64("reviewbot.claude.cost_usd", m.CostUSD),
			tracing.Int("reviewbot.claude.turns", m.NumTurns),
		)
		if m.Duration > 0 {
			sp.SetAttr(tracing.Int64("reviewbot.claude.cli_duration_ms", m.Duration.Milliseconds()))
		}
		if len(m.Models) > 0 {
			names := slices.Sorted(maps.Keys(m.Models))
			sp.SetAttr(tracing.String("gen_ai.response.model", strings.Join(names, ",")))
		}
	}
	if pi.Argv != nil {
		sp.SetAttr(tracing.Int("process.exit.code", pi.ExitCode))
	}
	if err != nil {
		class := errorClass(err)
		var rl *core.ErrClaudeRateLimited
		if errors.As(err, &rl) {
			class = "rate_limited"
		}
		sp.RecordError(err, class)
	} else {
		sp.SetStatus(tracing.StatusOK, "")
	}
	sp.End()
}

// execSpanName is "exec <program>" (the base name only: argv may carry
// paths and flags).
func execSpanName(argv []string) string {
	if len(argv) == 0 {
		return "exec"
	}
	return "exec " + filepath.Base(argv[0])
}
