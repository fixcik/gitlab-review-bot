package problems

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Record is one stored problem: one row per fingerprint.
type Record struct {
	Fingerprint string
	Kind        Kind
	Source      Source
	ProjectPath string
	Subject     string
	Detail      string
	FirstSeen   time.Time
	LastSeen    time.Time
	Count       int64
	LastMRIID   int64
	LastRunID   string
	AckedAt     time.Time // zero = not acknowledged
	// ResolvedAt: the bot saw the cause gone (Resolve); zero = still active.
	ResolvedAt time.Time
	NotifiedAt time.Time // zero = never notified
}

// Upserted is what UpsertProblem found before the update.
type Upserted struct {
	// New: no row with this fingerprint existed.
	New bool
	// Reappeared: the row was acknowledged (hidden) or resolved and is shown
	// again.
	Reappeared bool
	// NotifiedAt is when people were last told about it (zero = never).
	NotifiedAt time.Time
}

// Filter selects stored problems. Zero fields match everything.
type Filter struct {
	Kind    Kind
	Project string
	// Status: "" all, StatusNew (active: neither acknowledged nor resolved),
	// StatusAcked or StatusResolved.
	Status string
	Limit  int
}

// Status filter values.
const (
	StatusNew      = "new"
	StatusAcked    = "acked"
	StatusResolved = "resolved"
)

// Store keeps the problems (*store.SQLite).
type Store interface {
	// UpsertProblem inserts a row or updates it: last_seen, count+1,
	// detail, last MR and run; an acknowledged row becomes unacknowledged.
	UpsertProblem(ctx context.Context, r Record) (Upserted, error)
	MarkProblemsNotified(ctx context.Context, fingerprints []string, at time.Time) error
	ListProblems(ctx context.Context, f Filter) ([]Record, error)
	// AckProblem hides a problem until it is seen again; false when there is
	// no such unacknowledged row.
	AckProblem(ctx context.Context, fingerprint string, at time.Time) (bool, error)
	// ResolveProblem marks a problem fixed until it is seen again; false when
	// there is no such unresolved row.
	ResolveProblem(ctx context.Context, fingerprint string, at time.Time) (bool, error)
	PruneProblems(ctx context.Context, before time.Time) (int64, error)
}

// Notify modes (review.problems.notify_mode).
const (
	NotifySummary = "summary"
	NotifyNote    = "note"
)

// Defaults.
const (
	DefaultRenotify  = 168 * time.Hour
	DefaultRetention = 90 * 24 * time.Hour
)

// Options configure a Service.
type Options struct {
	Store Store
	// Notify are GitLab usernames (without "@") mentioned when a run hits a
	// new problem; empty = nobody.
	Notify []string
	// NotifyMode is NotifySummary (a cc line in the summary block) or
	// NotifyNote (a separate short note in the MR).
	NotifyMode string
	// Renotify: a problem seen again is mentioned again once this long has
	// passed since the last mention (default 168h).
	Renotify time.Duration
	// PageURL is the bot's external base URL; links point to PageURL +
	// "/problems" ("" = no link).
	PageURL string
	// Retention: rows not seen for this long are pruned (default 90 days).
	Retention time.Duration
	// Scrub removes secrets from stored text (diag scrubber); nil = none.
	Scrub func(string) string
	// RunID returns the id of the job's diagnostics run (/runs/<id>).
	RunID func(ctx context.Context) string
	// OnRecord is called for every persisted problem (fresh = new or
	// re-appeared after an acknowledgement): metrics.
	OnRecord func(p Problem, fresh bool)
	Now      func() time.Time
	Logger   *slog.Logger
}

// Service persists problems. A nil *Service records nothing.
type Service struct {
	o Options

	mu         sync.Mutex
	lastPruned time.Time
}

// New builds a Service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Renotify <= 0 {
		o.Renotify = DefaultRenotify
	}
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.NotifyMode != NotifyNote {
		o.NotifyMode = NotifySummary
	}
	o.PageURL = strings.TrimRight(strings.TrimSpace(o.PageURL), "/")
	var users []string
	for _, u := range o.Notify {
		if u = strings.TrimPrefix(strings.TrimSpace(u), "@"); u != "" {
			users = append(users, u)
		}
	}
	o.Notify = users
	return &Service{o: o}
}

// Notify returns the usernames to mention (without "@").
func (s *Service) Notify() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.o.Notify...)
}

// NotifyMode returns NotifySummary or NotifyNote.
func (s *Service) NotifyMode() string {
	if s == nil {
		return NotifySummary
	}
	return s.o.NotifyMode
}

// PageLink is the link to the /problems page ("" without page_url).
func (s *Service) PageLink() string {
	if s == nil || s.o.PageURL == "" {
		return ""
	}
	return s.o.PageURL + "/problems"
}

// Handler wraps a queue handler: every job collects its problems in the
// context, and whatever was not persisted yet is persisted when the
// handler returns (also after an error or a panic). A nil Service returns
// h unchanged.
func (s *Service) Handler(h core.Handler) core.Handler {
	if s == nil {
		return h
	}
	return func(ctx context.Context, job core.Job) error {
		ctx, run := s.Start(ctx, job)
		defer s.Finish(context.WithoutCancel(ctx), run)
		return h(ctx, job)
	}
}

// Start puts a fresh collector for job into ctx.
func (s *Service) Start(ctx context.Context, job core.Job) (context.Context, *Run) {
	if s == nil {
		return ctx, nil
	}
	run := &Run{svc: s, project: job.ProjectPath, mrIID: job.MRIID, byFP: map[string]*item{}}
	if s.o.RunID != nil {
		run.runID = s.o.RunID(ctx)
	}
	return With(ctx, run), run
}

// Finish persists what run collected and did not persist yet. It never
// panics.
func (s *Service) Finish(ctx context.Context, run *Run) {
	if s == nil || run == nil {
		return
	}
	defer func() {
		if p := recover(); p != nil {
			s.o.Logger.Error("problems: finish panicked", "panic", p)
		}
	}()
	run.commit(ctx)
}

// Record persists one problem outside a job (background work such as the
// system map scanner). It never fails.
func (s *Service) Record(ctx context.Context, p Problem) {
	if s == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.o.Logger.Error("problems: record panicked", "panic", r)
		}
	}()
	p = s.clean(p)
	if p.At.IsZero() {
		p.At = s.o.Now()
	}
	s.persist(ctx, p)
}

// clean scrubs and caps the texts of p.
func (s *Service) clean(p Problem) Problem {
	if p.Source == "" {
		p.Source = SourceBot
	}
	n := MaxSubjectRunes
	if p.Source == SourceModel {
		n = MaxModelSubjectRunes
	}
	if s.o.Scrub != nil {
		p.Subject, p.Detail = s.o.Scrub(p.Subject), s.o.Scrub(p.Detail)
	}
	p.Subject, p.Detail = CleanText(p.Subject, n), CleanText(p.Detail, MaxDetailRunes)
	return p
}

// persist stores p and reports what was there before (ok=false when the
// store failed or is missing).
func (s *Service) persist(ctx context.Context, p Problem) (Upserted, bool) {
	var up Upserted
	ok := false
	if s.o.Store != nil {
		var err error
		up, err = s.o.Store.UpsertProblem(ctx, Record{
			Fingerprint: p.Fingerprint(), Kind: p.Kind, Source: p.Source, ProjectPath: p.ProjectPath,
			Subject: p.Subject, Detail: p.Detail, FirstSeen: p.At, LastSeen: p.At, Count: 1,
			LastMRIID: p.MRIID, LastRunID: p.RunID,
		})
		ok = err == nil
		if err != nil {
			s.o.Logger.Warn("problems: store", "kind", p.Kind, "err", err)
		}
	}
	if s.o.OnRecord != nil {
		s.o.OnRecord(p, ok && (up.New || up.Reappeared))
	}
	if ok {
		s.prune(ctx)
	}
	return up, ok
}

// prune deletes rows not seen for the retention, at most once a day.
func (s *Service) prune(ctx context.Context) {
	now := s.o.Now()
	s.mu.Lock()
	if now.Sub(s.lastPruned) < 24*time.Hour {
		s.mu.Unlock()
		return
	}
	s.lastPruned = now
	s.mu.Unlock()
	if n, err := s.o.Store.PruneProblems(ctx, now.Add(-s.o.Retention)); err != nil {
		s.o.Logger.Warn("problems: prune", "err", err)
	} else if n > 0 {
		s.o.Logger.Info("problems: pruned", "rows", n)
	}
}

// MarkNotified records that people were told about these problems now.
func (s *Service) MarkNotified(ctx context.Context, outs []Outcome) {
	if s == nil || s.o.Store == nil {
		return
	}
	var fps []string
	for _, o := range outs {
		if o.Notify {
			fps = append(fps, o.Fingerprint)
		}
	}
	if len(fps) == 0 {
		return
	}
	if err := s.o.Store.MarkProblemsNotified(ctx, fps, s.o.Now()); err != nil {
		s.o.Logger.Warn("problems: mark notified", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Per-job collector
// ---------------------------------------------------------------------------

// Run collects the problems of one job.
type Run struct {
	svc     *Service
	project string
	mrIID   int64
	runID   string

	mu    sync.Mutex
	items []*item
	byFP  map[string]*item
	model int
}

type item struct {
	p         Problem
	fp        string
	persisted bool
	out       Outcome
}

// Outcome is a problem of this run after it was persisted.
type Outcome struct {
	Problem
	Fingerprint string
	Info        KindInfo
	New         bool // first time seen
	Reappeared  bool // seen again after an acknowledgement
	// Notify: the configured people should be told about it now (new,
	// re-appeared, never told, or told longer ago than renotify_after).
	Notify bool
}

type ctxKey struct{}

// With returns ctx carrying run.
func With(ctx context.Context, run *Run) context.Context {
	if run == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, run)
}

// From returns the collector in ctx (nil when problems are off).
func From(ctx context.Context) *Run {
	r, _ := ctx.Value(ctxKey{}).(*Run)
	return r
}

// Add records p in the job's collector (no-op without one). Kind is
// required; project, MR, run id and time default to the job's. The same
// fingerprint twice in a run keeps one entry with the last detail.
func Add(ctx context.Context, p Problem) {
	if run := From(ctx); run != nil {
		run.add(p)
	}
}

// Resolve marks the problem (kind, subject) of the job's project as fixed:
// the step that records it ran and found nothing wrong. It stays listed on
// /problems as "исправлена" and is active again when it is seen again. A
// problem the same job has just recorded is left alone. Best effort: a store
// failure is logged.
func Resolve(ctx context.Context, kind Kind, subject string) {
	run := From(ctx)
	if run == nil || run.svc == nil || run.svc.o.Store == nil {
		return
	}
	fp := Fingerprint(kind, run.project, CleanText(subject, MaxSubjectRunes))
	run.mu.Lock()
	_, recorded := run.byFP[fp]
	run.mu.Unlock()
	if recorded {
		return
	}
	if _, err := run.svc.o.Store.ResolveProblem(ctx, fp, run.svc.o.Now()); err != nil {
		run.svc.o.Logger.Warn("problems: resolve", "kind", kind, "err", err)
	}
}

// ErrModelLimit: the model already reported MaxModelPerRun problems.
var ErrModelLimit = fmt.Errorf("at most %d problems per review", MaxModelPerRun)

// AddModel records a problem the model reported: kind is a short model
// name ("tool_failed") or a model kind. Subject is required. It returns
// the stored kind, or an error for an unknown kind, an empty subject or
// the per-run cap. Without a collector it validates only.
func AddModel(ctx context.Context, kind, subject, detail string) (Kind, error) {
	k, ok := ModelKind(kind)
	if !ok {
		return "", fmt.Errorf("unknown kind %q (one of: %s)", CleanText(kind, 40), strings.Join(ModelNames(), ", "))
	}
	subject = CleanText(subject, MaxModelSubjectRunes)
	if subject == "" {
		return "", errors.New("subject is required")
	}
	run := From(ctx)
	if run == nil {
		return k, nil
	}
	run.mu.Lock()
	if run.model >= MaxModelPerRun {
		run.mu.Unlock()
		return "", ErrModelLimit
	}
	run.model++
	run.mu.Unlock()
	run.add(Problem{Kind: k, Source: SourceModel, Subject: subject, Detail: CleanText(detail, MaxDetailRunes)})
	return k, nil
}

func (r *Run) add(p Problem) {
	if p.Kind == "" {
		return
	}
	if p.ProjectPath == "" {
		p.ProjectPath = r.project
	}
	if p.MRIID == 0 {
		p.MRIID = r.mrIID
	}
	if p.RunID == "" {
		p.RunID = r.runID
	}
	if p.At.IsZero() {
		p.At = r.svc.o.Now()
	}
	p = r.svc.clean(p)
	fp := p.Fingerprint()
	r.mu.Lock()
	defer r.mu.Unlock()
	if it, ok := r.byFP[fp]; ok {
		if !it.persisted && p.Detail != "" {
			it.p.Detail = p.Detail
		}
		return
	}
	it := &item{p: p, fp: fp}
	r.byFP[fp] = it
	r.items = append(r.items, it)
}

// List returns the problems collected so far, in order.
func (r *Run) List() []Problem {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Problem, 0, len(r.items))
	for _, it := range r.items {
		out = append(out, it.p)
	}
	return out
}

// Commit persists the problems collected so far and returns every problem
// of the run (persisted now or earlier) with its outcome. Nil without a
// collector.
func Commit(ctx context.Context) []Outcome {
	run := From(ctx)
	if run == nil {
		return nil
	}
	return run.commit(ctx)
}

// ServiceOf returns the Service of the job's collector (nil without one).
func ServiceOf(ctx context.Context) *Service {
	if run := From(ctx); run != nil {
		return run.svc
	}
	return nil
}

func (r *Run) commit(ctx context.Context) []Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.svc
	out := make([]Outcome, 0, len(r.items))
	for _, it := range r.items {
		if !it.persisted {
			it.persisted = true
			up, ok := s.persist(ctx, it.p)
			it.out = Outcome{Problem: it.p, Fingerprint: it.fp, Info: InfoOf(it.p.Kind), New: ok && up.New, Reappeared: ok && up.Reappeared}
			// A store failure never triggers a mention (it would repeat on
			// every run while the store is broken). Neither does a problem
			// the model reported: its text comes from a run that reads
			// untrusted code, so a prompt injection could mint a new
			// fingerprint on every push and ping the notify users each time.
			// Model problems are still shown in the block and on /problems.
			it.out.Notify = ok && it.p.Source != SourceModel && len(s.o.Notify) > 0 &&
				(up.New || up.Reappeared || up.NotifiedAt.IsZero() || s.o.Now().Sub(up.NotifiedAt) >= s.o.Renotify)
		}
		out = append(out, it.out)
	}
	return out
}
