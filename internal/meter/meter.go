// Package meter records every Claude run (tokens, notional cost, duration)
// with its attribution: who caused it, in which repository and MR, and what
// kind of run it was. Attribution travels in the context: the job handler
// puts a mutable *Attribution there (With) and later code can enrich it
// (Update), e.g. with the MR author once the MR has been fetched.
package meter

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Run kinds shown on the usage page.
const (
	KindReview       = "review"
	KindChat         = "chat"
	KindVerification = "verification"
	KindLearning     = "learning"
	KindOther        = "other"
	// KindCI: the explanation of a failed pipeline (review.ci).
	KindCI = "ci"
	// KindMemory: compaction of a project's operational memory (light model).
	KindMemory = "memory"
	// KindMap: background extraction runs of the system map (light model,
	// internal/sysmap). Not charged to a user or an MR; ProjectPath is the
	// scanned repository.
	KindMap = "map"
)

// Attribution says whom and what a run is charged to.
type Attribution struct {
	Kind        string
	ProjectID   int64
	ProjectPath string
	MRIID       int64
	MRTitle     string
	Username    string // "" = unattributed
	UserID      int64
}

type holder struct {
	mu sync.Mutex
	a  Attribution
}

type ctxKey struct{}

// With returns a context carrying a fresh, mutable attribution a. Runs made
// with the returned context (or contexts derived from it) are charged to it.
func With(ctx context.Context, a Attribution) context.Context {
	return context.WithValue(ctx, ctxKey{}, &holder{a: a})
}

// Update changes the attribution in ctx in place, so the change is seen by
// every context derived from the one With returned. No-op without one.
func Update(ctx context.Context, f func(*Attribution)) {
	h, _ := ctx.Value(ctxKey{}).(*holder)
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.a)
}

// From returns the current attribution in ctx.
func From(ctx context.Context) (Attribution, bool) {
	h, _ := ctx.Value(ctxKey{}).(*holder)
	if h == nil {
		return Attribution{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.a, true
}

// KindOf is the run kind of the attribution in ctx (KindOther without one
// or with an empty kind).
func KindOf(ctx context.Context) string {
	if a, ok := From(ctx); ok && a.Kind != "" {
		return a.Kind
	}
	return KindOther
}

// RunMetrics is what a finished Claude run used: its result, or the partial
// result of a *core.FailedRun; nil when there is neither.
func RunMetrics(res *core.RunResult, err error) *core.RunResult {
	if res != nil {
		return res
	}
	var fr *core.FailedRun
	if errors.As(err, &fr) {
		return &fr.Partial
	}
	return nil
}

// Store is where runs go (*store.SQLite).
type Store interface {
	RecordRun(ctx context.Context, r ClaudeRun) error
	PruneRuns(ctx context.Context, before time.Time) (int64, error)
}

// Options tune Wrap.
type Options struct {
	// Retention: runs older than this are pruned (at most once a day, after
	// a recorded run). Default 365 days; negative disables pruning.
	Retention time.Duration
}

// DefaultRetention is how long runs are kept by default.
const DefaultRetention = 365 * 24 * time.Hour

// Runner is the metering decorator returned by Wrap.
//
// Every method of core.Runner must be forwarded here. Run is metered;
// everything else passes through. If core.Runner gains a method, add a
// one-line forwarder like Exec's below.
type Runner struct {
	next core.Runner
	st   Store
	now  func() time.Time
	log  *slog.Logger
	ret  time.Duration

	mu         sync.Mutex
	lastPruned time.Time
}

var _ core.Runner = (*Runner)(nil)

// Wrap returns r with every Run recorded in st. Recording problems are
// logged and never fail the run.
func Wrap(r core.Runner, st Store, now func() time.Time, log *slog.Logger, opts ...Options) *Runner {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Runner{next: r, st: st, now: now, log: log, ret: DefaultRetention}
	for _, o := range opts {
		if o.Retention != 0 {
			m.ret = o.Retention
		}
	}
	return m
}

// Exec passes through (deterministic checkers, no Claude tokens).
func (m *Runner) Exec(ctx context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	return m.next.Exec(ctx, req)
}

// Run runs Claude and records the run, successful or not.
func (m *Runner) Run(ctx context.Context, req core.RunRequest) (*core.RunResult, error) {
	start := m.now()
	res, err := m.next.Run(ctx, req)
	m.record(ctx, req, start, res, err)
	return res, err
}

func (m *Runner) record(ctx context.Context, req core.RunRequest, start time.Time, res *core.RunResult, runErr error) {
	a, _ := From(ctx)
	if a.Kind == "" {
		a.Kind = KindOther
	}
	run := ClaudeRun{
		At: start, Kind: a.Kind, ProjectID: a.ProjectID, ProjectPath: a.ProjectPath,
		MRIID: a.MRIID, MRTitle: a.MRTitle, Username: a.Username, UserID: a.UserID,
		Model: req.Model, OK: runErr == nil,
	}
	if metrics := RunMetrics(res, runErr); metrics != nil {
		run.Usage, run.CostUSD, run.Duration, run.NumTurns = metrics.Usage, metrics.CostUSD, metrics.Duration, metrics.NumTurns
		run.Models = metrics.Models
	}
	if run.Duration == 0 {
		run.Duration = m.now().Sub(start)
	}
	if runErr != nil {
		run.ErrorClass = ErrorClass(runErr)
	}
	// Record even when the job's context was cancelled: the tokens are spent.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := m.st.RecordRun(rctx, run); err != nil {
		m.log.Warn("meter: record claude run", "err", err)
		return
	}
	m.maybePrune(rctx)
}

func (m *Runner) maybePrune(ctx context.Context) {
	if m.ret < 0 {
		return
	}
	now := m.now()
	m.mu.Lock()
	if !m.lastPruned.IsZero() && now.Sub(m.lastPruned) < 24*time.Hour {
		m.mu.Unlock()
		return
	}
	m.lastPruned = now
	m.mu.Unlock()
	n, err := m.st.PruneRuns(ctx, now.Add(-m.ret))
	if err != nil {
		m.log.Warn("meter: prune claude runs", "err", err)
		return
	}
	if n > 0 {
		m.log.Info("meter: pruned old claude runs", "deleted", n)
	}
}

// ErrorClass maps a Runner error to a short stable class for the table:
// rate_limited, timeout, canceled, max_turns, failed.
func ErrorClass(err error) string {
	var rl *core.ErrClaudeRateLimited
	switch {
	case err == nil:
		return ""
	case errors.As(err, &rl):
		return "rate_limited"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case strings.Contains(err.Error(), "error_max_turns"):
		return "max_turns"
	default:
		return "failed"
	}
}

// KindForJob maps a job kind to a run kind (core.KindInfo.RunKind). A kind
// outside the kind table (an old persisted row) is "learning" when its name
// says so, "other" otherwise.
func KindForJob(k core.JobKind) string {
	if info, ok := k.Info(); ok && info.RunKind != "" {
		return info.RunKind
	}
	if strings.Contains(string(k), "learn") {
		return KindLearning
	}
	return KindOther
}

// ForJob is the initial attribution of a job: manual jobs are charged to
// the user who issued the command; auto jobs get the MR author later (see
// review flow, meter.Update after GetMR).
func ForJob(job core.Job) Attribution {
	a := Attribution{Kind: KindForJob(job.Kind), ProjectID: job.ProjectID, ProjectPath: job.ProjectPath, MRIID: job.MRIID}
	if job.Kind.Manual() {
		a.Username, a.UserID = job.Username, job.UserID
	}
	return a
}
