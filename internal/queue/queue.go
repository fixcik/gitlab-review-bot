// Package queue implements core.Queue: an in-process job queue with per-MR
// serialization, debounce of automatic reviews, priorities, retries, a
// durable copy of every job in a Persister (restored after a restart) and a
// graceful drain on shutdown.
package queue

import (
	"log/slog"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Options configures a Queue.
type Options struct {
	Concurrency int              // max handlers running at once; values < 1 mean 1
	Debounce    time.Duration    // delay before an auto job becomes ready
	Logger      *slog.Logger     // nil discards logs
	Now         func() time.Time // optional, for tests; defaults to time.Now
	// Persist, when set, keeps every job across restarts: pending automatic
	// reviews (debounced or postponed by RetryLater, one per MR) and every
	// other job (commands, chat, thread replies, refreshes, pipelines), queued
	// or running. See Persister and docs/DESIGN.md "Queue durability".
	Persist Persister
	// ShutdownGrace: when the context of Run is cancelled, dispatching stops
	// at once and the running handlers get this long to finish before their
	// context is cancelled (they keep their rows and run again after a
	// restart). 0 cancels them at once.
	ShutdownGrace time.Duration
	// ManualMaxAge: on restore, a persisted job that is not an automatic
	// review and was enqueued longer ago than this is dropped (logged,
	// OnRestore RestoreExpired). 0 = no limit.
	ManualMaxAge time.Duration
	// OnRestore, when set, is called once for every persisted job found on
	// restore, with what became of it (RestoreQueued, RestoreReplayed,
	// RestoreExpired, RestoreAnswered).
	OnRestore func(kind core.JobKind, result string)
	// OnDrop, when set, is called for every persisted job restore drops as
	// RestoreAnswered or RestoreExpired (main: the reviewer takes 👀 / ⏳ off
	// its note). Called with the queue's lock held: it must not block or
	// call the queue.
	OnDrop func(job core.Job, result string)
}

// Queue is a core.Queue.
//
// Scheduling rules:
//   - At most one job per MR key runs at a time; other jobs of that MR wait.
//   - Auto jobs are debounced per MR: a new auto job replaces the pending (not
//     running) auto job of that MR, including one waiting for a retry, and is
//     ready at now+Debounce. ForceFull is sticky across replacements.
//   - Manual jobs are ready immediately and keep FIFO order per MR. A
//     manual job waiting for its RetryLater time does not hold the MR's
//     later jobs (they run meanwhile, one at a time); when its time comes
//     it goes before them again (it keeps its seq).
//   - Among ready jobs, auto jobs go first, then by ready time, then FIFO.
//   - A handler returning *core.RetryLater re-schedules the job at At, unless
//     a newer auto job for the same MR is already pending (it supersedes the
//     retry and inherits ForceFull).
//   - Shutdown (the context of Run cancelled) stops dispatching and drains:
//     running handlers get Options.ShutdownGrace to finish, then their
//     context is cancelled. Enqueue keeps working (and persisting) during
//     and after the drain.
type Queue struct {
	concurrency int
	debounce    time.Duration
	log         *slog.Logger
	now         func() time.Time
	persist     Persister
	grace       time.Duration
	maxAge      time.Duration
	onRestore   func(core.JobKind, string)
	onDrop      func(core.Job, string)

	mu          sync.Mutex
	pending     []*entry
	autoPending map[string]*entry // pending auto job per MR key
	running     map[string]bool   // MR keys with a running handler
	nRunning    int
	seq         uint64
	started     bool
	draining    bool       // Run's context is done, running jobs are finishing
	restored    bool       // persisted jobs loaded (Restore, first Enqueue or Run)
	restoredJ   []core.Job // the jobs restore queued again (Restore)
	seenJ       []core.Job // the jobs restore dropped or found finished (Restore)

	wake      chan struct{}
	abort     chan struct{} // closed by Abort: cancel running jobs now
	abortOnce sync.Once
}

var _ core.Queue = (*Queue)(nil)

// New creates a Queue.
func New(opts Options) *Queue {
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if opts.Debounce < 0 {
		opts.Debounce = 0
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Queue{
		concurrency: opts.Concurrency,
		debounce:    opts.Debounce,
		log:         opts.Logger,
		now:         opts.Now,
		persist:     opts.Persist,
		grace:       max(opts.ShutdownGrace, 0),
		maxAge:      max(opts.ManualMaxAge, 0),
		onRestore:   opts.OnRestore,
		onDrop:      opts.OnDrop,
		autoPending: make(map[string]*entry),
		running:     make(map[string]bool),
		wake:        make(chan struct{}, 1),
		abort:       make(chan struct{}),
	}
}

// Abort cancels the running handlers of a draining Run at once instead of
// waiting for the rest of the grace period (a second SIGTERM/SIGINT). Their
// rows are kept, like after the grace. Safe to call at any time; it only
// has an effect on a drain.
func (q *Queue) Abort() { q.abortOnce.Do(func() { close(q.abort) }) }

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Enqueue adds a job. Safe for concurrent use, before or after Run starts.
func (q *Queue) Enqueue(job core.Job) {
	now := q.now()
	if job.EnqueuedAt.IsZero() {
		job.EnqueuedAt = now
	}
	key := job.MRKey()
	auto := job.Kind == core.JobAutoReview

	q.mu.Lock()
	// Load persisted jobs first, so a new push debounces over a restored
	// review of the same MR (and inherits its ForceFull).
	q.restoreLocked()
	if old, ok := q.autoPending[key]; auto && ok {
		job.ForceFull = job.ForceFull || old.job.ForceFull
		old.job = job
		old.readyAt = now.Add(q.debounce)
		old.retry = false
		old.seq = q.nextSeqLocked()
		q.saveLocked(old)
		q.mu.Unlock()
		q.log.Debug("queue: auto job debounced", "mr", key, "head", job.HeadSHA)
		q.signal()
		return
	}
	e := &entry{job: job, key: key, auto: auto, readyAt: now, seq: q.nextSeqLocked()}
	if auto {
		e.readyAt = now.Add(q.debounce)
		q.autoPending[key] = e
		q.saveLocked(e)
	} else {
		q.addJobLocked(e)
	}
	q.pending = append(q.pending, e)
	q.mu.Unlock()
	q.log.Debug("queue: job enqueued", "mr", key, "kind", job.Kind)
	q.signal()
}

func (q *Queue) nextSeqLocked() uint64 {
	q.seq++
	return q.seq
}
