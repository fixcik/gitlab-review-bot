package queue

import (
	"context"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// What restore did with a persisted job (Options.OnRestore).
const (
	// RestoreQueued: the job had not started (or waited for a retry); it is
	// queued again as it was.
	RestoreQueued = "queued"
	// RestoreReplayed: the job was running when the process stopped and had
	// not answered yet; it runs again.
	RestoreReplayed = "replayed"
	// RestoreExpired: older than Options.ManualMaxAge; dropped.
	RestoreExpired = "expired"
	// RestoreAnswered: the job was running when the process stopped but had
	// already posted its final reply; dropped (a replay would answer
	// twice).
	RestoreAnswered = "answered"
	// RestoreInvalid: an unusable row (unreadable job, no project, no MR —
	// a pipeline job may have its pipeline instead); dropped.
	RestoreInvalid = "invalid"
)

// Pending is a persisted automatic review: the job and when it is due.
type Pending struct {
	Job     core.Job
	ReadyAt time.Time
}

// Stored is a persisted job that is not an automatic review: one row per
// job. ID is the store's row id (assigned by AddJob, never reused), so
// ordering by it restores the enqueue (FIFO) order.
type Stored struct {
	ID      int64
	Job     core.Job
	ReadyAt time.Time
	// StartedAt: when a handler last started the job (zero: never, or a
	// RetryLater reset it: the handler asked to run it again).
	StartedAt time.Time
	// Answered: during the attempt that started at StartedAt the job's
	// final reply was posted (core.RunState.MarkAnswered). A restore drops
	// such a job instead of replaying it.
	Answered bool
	// Charged: an attempt committed a Claude run against the user's limits
	// (WrapLimiter). A restored job then skips the per-user limits and the
	// MR cooldown and books its runs without the user (WrapLimiter).
	Charged bool
}

// Persister keeps the queue across restarts.
//
// Automatic reviews: at most one row per MR (the newest replaces the older
// one, like the in-memory debounce). The queue saves an auto job when it is
// enqueued, debounced or postponed (RetryLater) and deletes it once a
// handler finished it for good (success or a non-retry error). A job that
// was running when the process stopped keeps its row and runs again.
//
// Every other job: one row per job (AddJob at enqueue), updated when it
// starts, answers, is charged or is postponed (UpdateJob), finished once a
// handler finished it for good or restore dropped it (FinishJob: the row
// goes, a marker of the job stays for DoneTTL). A job interrupted by
// shutdown keeps its row; restore decides whether it is replayed (see
// Stored).
//
// Restore (Restore, or the first Enqueue or Run) reloads every row once per
// process.
type Persister interface {
	SavePending(ctx context.Context, p Pending) error
	DeletePending(ctx context.Context, projectID, mrIID int64) error
	ListPending(ctx context.Context) ([]Pending, error)

	AddJob(ctx context.Context, s Stored) (int64, error)
	UpdateJob(ctx context.Context, s Stored) error
	// FinishJob deletes the row of a job that is done (finished, or dropped
	// by restore) and keeps a marker of its job, finished at at, for the
	// event dedup of the next restart (ListDone); markers finished before
	// at - DoneTTL are forgotten.
	FinishJob(ctx context.Context, id int64, at time.Time) error
	// ListJobs returns every stored job, by ID.
	ListJobs(ctx context.Context) ([]Stored, error)
	// ListDone returns the jobs finished at or after since (FinishJob).
	ListDone(ctx context.Context, since time.Time) ([]core.Job, error)
}

// DoneTTL is how long a finished job is remembered (Persister.FinishJob)
// for the event dedup after a restart: with gitlab.events "both" the
// poller's first rounds after a start see again the comments the webhook
// delivered (and the bot answered) since the poller last read their MR;
// Restore returns these jobs so the dedup drops them. The same as the
// dedup's own window (cmd/reviewbot startEventSources).
const DoneTTL = 24 * time.Hour

// persistTimeout bounds one persistence call.
const persistTimeout = 5 * time.Second

// saveLocked persists a pending auto job (best effort). Called with q.mu
// held so that saves and deletes of one MR stay in order.
func (q *Queue) saveLocked(e *entry) {
	if q.persist == nil || !e.auto {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if err := q.persist.SavePending(ctx, Pending{Job: e.job, ReadyAt: e.readyAt}); err != nil {
		q.log.Warn("queue: persist pending job", "mr", e.key, "err", err)
	}
}

// deleteLocked forgets the persisted auto job of an MR (best effort).
func (q *Queue) deleteLocked(e *entry) {
	if q.persist == nil || !e.auto {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if err := q.persist.DeletePending(ctx, e.job.ProjectID, e.job.MRIID); err != nil {
		q.log.Warn("queue: delete persisted job", "mr", e.key, "err", err)
	}
}

// Restore loads the persisted jobs now (once per process; Enqueue and Run
// do it too) and returns every job whose event is already handled or
// queued: the jobs it queued again, the rows it dropped (answered, expired,
// invalid) and the jobs finished within DoneTTL before the restart. The
// caller tells the event sources about them (the poller's dedup: with
// gitlab.events both, the poller would otherwise enqueue those comments
// again after a restart and the bot would answer twice).
func (q *Queue) Restore() []core.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.restoreLocked()
	return append(append([]core.Job(nil), q.restoredJ...), q.seenJ...)
}

// restoreLocked re-queues the persisted jobs, once per process (Restore,
// first Enqueue or Run): the auto jobs (one per MR) at their saved ready
// time — it runs before anything is enqueued, so a second row of an MR
// can only be a duplicate from the Persister: the first row listed wins
// and ForceFull is merged into it — and every other job in its stored
// order (restoreJobsLocked).
func (q *Queue) restoreLocked() {
	if q.persist == nil || q.restored {
		return
	}
	q.restored = true
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	list, err := q.persist.ListPending(ctx)
	if err != nil {
		q.log.Warn("queue: load persisted jobs", "err", err)
	}
	restored := 0
	for _, p := range list {
		if p.Job.Kind != core.JobAutoReview {
			continue
		}
		key := p.Job.MRKey()
		if cur, ok := q.autoPending[key]; ok {
			if p.Job.ForceFull && !cur.job.ForceFull {
				cur.job.ForceFull = true
				q.saveLocked(cur)
			}
			continue
		}
		if p.Job.EnqueuedAt.IsZero() {
			p.Job.EnqueuedAt = q.now()
		}
		e := &entry{job: p.Job, key: key, auto: true, readyAt: p.ReadyAt, seq: q.nextSeqLocked()}
		q.autoPending[key] = e
		q.pending = append(q.pending, e)
		q.restoredJ = append(q.restoredJ, p.Job)
		q.observeRestore(p.Job.Kind, RestoreQueued)
		restored++
	}
	if restored > 0 {
		q.log.Info("queue: restored pending automatic reviews", "count", restored)
	}
	q.restoreJobsLocked(ctx)
}

func (q *Queue) observeRestore(kind core.JobKind, result string) {
	if q.onRestore != nil {
		q.onRestore(kind, result)
	}
}
