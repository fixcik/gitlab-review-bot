package main

import (
	"log/slog"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Instrumentation: every decorator around the bot's collaborators is applied
// in this file, so the nesting order lives in one place. Inside -> outside
// (the outermost wrapper sees a call first):
//
//	Runner         claude -> meter (token accounting, /usage) -> diag (steps,
//	               transcripts, spans) -> telemetry (run metrics)
//	               [instrumentRunner; shared by the reviewer and the system map]
//	GitLab         client -> queue (answered marker, replay rule) -> diag
//	               -> review's per-job memo (inside review.New: memo hits
//	               record no step)
//	Limiter        limits -> queue (charged marker) -> telemetry -> diag
//	Repos          repo.Manager -> diag
//	Moderator      service client -> diag
//	Labels         bot client -> diag; ServiceLabels: service client -> diag
//	Compat, Lint   checker on Deps.Runner -> telemetry
//	Handler        reviewer.Handle -> problems collector -> telemetry -> diag
//	               run -> job span [instrumentHandler]: the job span is
//	               outermost, so the diagnostics run records its trace id; the
//	               problems collector is inside the run (problems link to
//	               /runs/<id>)
//
// GitLab HTTP metrics come from the clients' observer (observeGitLab), not
// from a wrapper. Optional GitLab capabilities found with core.GitLabAs
// unwrap past the diag wrapper and record no step (otelhttp).

// instruments selects the sinks of the decorators; each may be off.
type instruments struct {
	// hooks: diagnostics steps (hooks.Rec, nil = off) and trace spans
	// (hooks.Trace).
	hooks diag.Hooks
	// tm: Prometheus metrics; nil = off (every telemetry wrapper is then a
	// no-op).
	tm *telemetry.Metrics
}

// instrumentRunner wraps the Claude runner (see the order above). st
// receives the metered runs.
func instrumentRunner(in instruments, cfg *config.Service, r core.Runner, st meter.Store, logger *slog.Logger) core.Runner {
	r = meter.Wrap(r, meter.Store(st), time.Now, logger, meter.Options{Retention: cfg.Usage.RetentionD()})
	r = in.hooks.WrapRunner(r) // exec/claude steps, transcripts and spans
	return in.tm.WrapRunner(r) // claude runs/tokens/cost metrics
}

// instrument wraps the reviewer's collaborators in d (see the order above)
// and fills the metric observers and the default compat/lint checkers (the
// ones review would build on Deps.Runner) when metrics are on. Deps.Runner
// is expected to be instrumented already (instrumentRunner). Nil
// collaborators stay nil.
func instrument(in instruments, cfg *config.Service, d review.Deps) review.Deps {
	// The queue's marker: a run booked against the job's user is recorded
	// on the persisted job (replay after a restart). The job's final reply
	// marks it answered itself (env.Reply, core.RunState.MarkAnswered).
	if d.Limiter != nil {
		d.Limiter = queue.WrapLimiter(d.Limiter)
	}
	// Metrics.
	if in.tm != nil {
		d.Limiter = in.tm.WrapLimiter(d.Limiter)
		if d.Verify == nil {
			d.Verify = in.tm // reviewbot_verify_* (review/verify)
		}
		if d.CI == nil {
			d.CI = in.tm // reviewbot_ci_* (review/pipeline)
		}
		if d.Compat == nil {
			d.Compat = in.tm.WrapCompat(&compat.Checker{Runner: d.Runner, Timeout: cfg.Review.CompatTimeout.D(), Logger: d.Logger})
		}
		if d.Lint == nil {
			d.Lint = in.tm.WrapLint(&lint.Checker{Runner: d.Runner, Timeout: cfg.Review.Lint.Timeout.D(), Logger: d.Logger})
		}
	}
	// Diagnostics steps and spans (outermost).
	h := in.hooks
	d.GitLab = h.WrapGitLab(d.GitLab)
	d.Limiter = h.WrapLimiter(d.Limiter)
	d.Repos = h.WrapRepos(d.Repos)
	if d.Moderator != nil {
		d.Moderator = h.WrapModerator(d.Moderator)
	}
	if d.Labels != nil {
		d.Labels = h.WrapLabels(d.Labels, false)
	}
	if d.ServiceLabels != nil {
		d.ServiceLabels = h.WrapLabels(d.ServiceLabels, true)
	}
	return d
}

// instrumentHandler wraps the reviewer's job handler (see the order above).
// rec and probs may be nil (diagnostics / review.problems off).
func instrumentHandler(in instruments, tr *tracing.Tracer, active *tracing.Active, rec *diag.Recorder, probs *problems.Service,
	h core.Handler) core.Handler {
	return diag.TraceJobs(tr, active, rec.Handler(in.tm.Handler(probs.Handler(h))))
}
