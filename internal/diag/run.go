package diag

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Run is the record of one job being handled. A nil *Run records nothing.
type Run struct {
	rec    *Recorder
	dir    string // absolute
	relDir string // <YYYYMMDD>/<id>
	mrKey  string
	span   *tracing.Span // the job's root span (nil: not traced)

	mu         sync.Mutex
	r          Record
	safe       []string // strings never scrubbed (known commit SHAs)
	skip       string   // reason from a "skipping" log line
	tBytes     int64    // transcript bytes written
	lastWrite  time.Time
	finished   bool
	writeError bool
}

type ctxKey struct{}

// FromContext returns the Run in ctx, or nil.
func FromContext(ctx context.Context) *Run {
	r, _ := ctx.Value(ctxKey{}).(*Run)
	return r
}

// WithRun returns ctx carrying run.
func WithRun(ctx context.Context, run *Run) context.Context {
	if run == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, run)
}

// Handler wraps a queue handler: every job gets a Run that is finished with
// the handler's result. With a nil Recorder it returns h unchanged.
func (rec *Recorder) Handler(h core.Handler) core.Handler {
	if rec == nil {
		return h
	}
	return func(ctx context.Context, job core.Job) error {
		ctx, run := rec.Start(ctx, job)
		var err error
		defer func() {
			if p := recover(); p != nil {
				run.Finish(fmt.Errorf("panic: %v", p))
				panic(p)
			}
			run.Finish(err)
		}()
		err = h(ctx, job)
		return err
	}
}

// Start begins the record of job. It never fails: problems are logged and
// the returned Run still collects data in memory.
func (rec *Recorder) Start(ctx context.Context, job core.Job) (context.Context, *Run) {
	if rec == nil {
		return ctx, nil
	}
	now := rec.o.Now()
	id := newID(now)
	rel := filepath.Join(now.UTC().Format("20060102"), id)
	run := &Run{rec: rec, dir: filepath.Join(rec.o.Root, rel), relDir: rel, mrKey: job.MRKey(), span: tracing.SpanFromContext(ctx)}
	run.r = Record{
		Version: RecordVersion, ID: id, StartedAt: now, Status: StatusRunning,
		Attempt: rec.nextAttempt(job, now),
		Job: JobInfo{
			Kind: string(job.Kind), ProjectID: job.ProjectID, ProjectPath: job.ProjectPath, MRIID: job.MRIID,
			HeadSHA: job.HeadSHA, Username: job.Username, UserID: job.UserID, DiscussionID: job.DiscussionID,
			NoteID: job.NoteID, Question: rec.scrub.str(job.Question, nil), ForceFull: job.ForceFull, EnqueuedAt: job.EnqueuedAt,
		},
		Steps: []TimelineStep{}, Claude: []ClaudeRun{},
	}
	if run.span.IsRecording() {
		run.r.TraceID = run.span.TraceID()
		run.span.SetAttr(tracing.String("reviewbot.run_id", id), tracing.Int("reviewbot.job.attempt", run.r.Attempt))
	}
	run.r.Job.MRURL = rec.mrURL(job.ProjectPath, job.MRIID)
	if job.HeadSHA != "" {
		run.safe = append(run.safe, job.HeadSHA)
	}
	if !job.EnqueuedAt.IsZero() && job.EnqueuedAt.Before(now) {
		wait := now.Sub(job.EnqueuedAt)
		detail := "ожидание в очереди (debounce, занятые воркеры, другая задача этого MR)"
		if run.r.Attempt > 1 {
			detail = fmt.Sprintf("повторный запуск №%d после отложенной попытки; время считается с момента постановки в очередь", run.r.Attempt)
		}
		run.r.Steps = append(run.r.Steps, TimelineStep{N: 1, Kind: KindQueue, Name: "очередь", Start: job.EnqueuedAt,
			DurationMS: wait.Milliseconds(), Status: StepOK, Detail: detail})
	}
	if err := os.MkdirAll(run.dir, 0o750); err != nil {
		rec.log.Warn("diag: create run dir", "err", err)
		run.writeError = true
	}
	rec.mu.Lock()
	rec.active[run.mrKey] = run
	rec.mu.Unlock()
	run.mu.Lock()
	run.flushLocked(true)
	run.mu.Unlock()
	return WithRun(ctx, run), run
}

func (rec *Recorder) nextAttempt(job core.Job, now time.Time) int {
	key := fmt.Sprintf("%s|%s|%d|%d|%s", job.Kind, job.MRKey(), job.EnqueuedAt.UnixNano(), job.NoteID, job.HeadSHA)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for k, a := range rec.attempts {
		if now.Sub(a.last) > rec.o.Retention {
			delete(rec.attempts, k)
		}
	}
	a := rec.attempts[key]
	a.n++
	a.last = now
	rec.attempts[key] = a
	return a.n
}

func (rec *Recorder) mrURL(projectPath string, iid int64) string {
	if rec.o.GitLabURL == "" || projectPath == "" || iid <= 0 {
		return ""
	}
	return fmt.Sprintf("%s/%s/-/merge_requests/%d", rec.o.GitLabURL, strings.Trim(projectPath, "/"), iid)
}

func newID(t time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return t.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// ID returns the run id ("" for a nil Run).
func (run *Run) ID() string {
	if run == nil {
		return ""
	}
	return run.r.ID
}

// Finish closes the record with the handler's result:
// nil -> ok (or skipped when the job logged a skip reason and ran no
// Claude session), *core.RetryLater -> postponed, a cancelled context ->
// interrupted, anything else -> failed.
func (run *Run) Finish(err error) {
	if run == nil {
		return
	}
	rec := run.rec
	now := rec.o.Now()
	run.mu.Lock()
	if run.finished {
		run.mu.Unlock()
		return
	}
	run.finished = true
	r := &run.r
	r.FinishedAt = now
	r.DurationMS = now.Sub(r.StartedAt).Milliseconds()
	r.Status = Outcome(err)
	switch r.Status {
	case StatusOK:
		if run.skippedLocked() {
			r.Status, r.Reason = StatusSkipped, run.skip
		}
	case StatusPostponed:
		var rl *core.RetryLater
		errors.As(err, &rl)
		r.RetryAt, r.Reason = rl.At, rec.scrub.str(rl.Reason, run.safe)
	default: // interrupted, failed
		r.Error, r.ErrorClass = rec.scrub.str(err.Error(), run.safe), errorClass(err)
	}
	for i := range r.Steps {
		if r.Steps[i].Status == StepRunning {
			r.Steps[i].Status = StepError
			r.Steps[i].Error = "не завершён к концу задачи"
		}
	}
	run.flushLocked(true)
	status, reason := r.Status, r.Reason
	run.mu.Unlock()
	// The job span (trace.go) gets the same outcome as /runs.
	run.span.SetAttr(tracing.String(AttrJobResult, status))
	if reason != "" {
		run.span.SetAttr(tracing.String(AttrJobReason, reason))
	}

	rec.mu.Lock()
	if rec.active[run.mrKey] == run {
		delete(rec.active, run.mrKey)
	}
	rec.mu.Unlock()
	rec.enforceTotal(context.Background(), run.r.ID)
}

// addSafe marks strings (commit SHAs from GitLab) as never scrubbed.
func (run *Run) addSafe(ss ...string) {
	if run == nil {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	for _, s := range ss {
		if len(s) >= 7 && !slices.Contains(run.safe, s) {
			run.safe = append(run.safe, s)
		}
	}
}

// setMR records what GetMR returned.
func (run *Run) setMR(mr *core.MergeRequest) {
	if run == nil || mr == nil {
		return
	}
	run.addSafe(mr.DiffRefs.BaseSHA, mr.DiffRefs.StartSHA, mr.DiffRefs.HeadSHA)
	run.mu.Lock()
	defer run.mu.Unlock()
	j := &run.r.Job
	j.MRTitle = run.rec.scrub.str(mr.Title, run.safe)
	if mr.WebURL != "" {
		j.MRURL = mr.WebURL
	}
	j.MRAuthor = mr.AuthorUsername
	if mr.DiffRefs.HeadSHA != "" {
		j.HeadSHA = mr.DiffRefs.HeadSHA
	}
}

// setMRSpan puts what GetMR returned on the job span (traced jobs, with
// or without diagnostics): ids only.
func setMRSpan(ctx context.Context, mr *core.MergeRequest) {
	if mr == nil {
		return
	}
	sp := jobSpan(ctx)
	if mr.DiffRefs.HeadSHA != "" {
		sp.SetAttr(tracing.String("vcs.head_sha", mr.DiffRefs.HeadSHA))
	}
	sp.SetAttr(tracing.String("gitlab.mr.state", mr.State), tracing.Bool("gitlab.mr.draft", mr.Draft))
}

// Span attributes of the review's model route (SetRoute).
const (
	AttrReviewRoute = "reviewbot.review.route" // light | normal | heavy
	AttrReviewModel = "reviewbot.review.model"
)

// SetRoute records the model route of the review in ctx: on its run
// record (/runs) and on the job span. Without a Run or a span it is a
// no-op for that side.
func SetRoute(ctx context.Context, route, model, reason string) {
	jobSpan(ctx).SetAttr(tracing.String(AttrReviewRoute, route), tracing.String(AttrReviewModel, model))
	run := FromContext(ctx)
	if run == nil {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	run.r.Route = &RouteInfo{Route: route, Model: model, Reason: run.rec.scrub.str(reason, run.safe)}
}
