package queue

import (
	"context"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Durable jobs that are not automatic reviews (commands, chat, thread
// replies, refreshes, pipelines): one Persister row per job, see Stored.
//
// Replay rule after a restart (restoreJobsLocked), the same for every kind:
//   - never started (or postponed by RetryLater): queued again as it was
//     (a charged one stays admitted, see WrapLimiter);
//   - started and interrupted by shutdown: replayed, unless the job's final
//     reply was out during that attempt (Stored.Answered, set through
//     core.RunState.MarkAnswered by the reviewer's reply): then it is
//     dropped, because a replay would answer twice;
//   - enqueued longer ago than Options.ManualMaxAge: dropped.
//
// Kinds without a reply (refresh_title, refresh_status, pipeline) are
// never marked answered: their handlers re-read what they act on and are
// idempotent, so they are always replayed. docs/DESIGN.md "Queue
// durability" has the table per kind.

// addJobLocked persists a new non-auto job (best effort: without a row the
// job still runs, it is just not durable).
func (q *Queue) addJobLocked(e *entry) {
	if q.persist == nil || e.auto {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	id, err := q.persist.AddJob(ctx, q.storedLocked(e))
	if err != nil {
		q.log.Warn("queue: persist job", "mr", e.key, "kind", e.job.Kind, "err", err)
		return
	}
	e.rowID = id
}

// updateJobLocked writes the persisted state of a non-auto job.
func (q *Queue) updateJobLocked(e *entry) {
	if q.persist == nil || e.rowID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if err := q.persist.UpdateJob(ctx, q.storedLocked(e)); err != nil {
		q.log.Warn("queue: update persisted job", "mr", e.key, "kind", e.job.Kind, "err", err)
	}
}

// finishJobLocked forgets a finished non-auto job (a marker of it stays
// for DoneTTL, Persister.FinishJob).
func (q *Queue) finishJobLocked(e *entry) {
	if q.persist == nil || e.rowID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if err := q.persist.FinishJob(ctx, e.rowID, q.now()); err != nil {
		q.log.Warn("queue: delete persisted job", "mr", e.key, "kind", e.job.Kind, "err", err)
	}
	e.rowID = 0
}

// markStartedLocked records that a handler is about to start e (before the
// handler runs, so a crash right after it is seen as a started attempt).
func (q *Queue) markStartedLocked(e *entry, now time.Time) {
	if e.rowID == 0 {
		return
	}
	e.startedAt, e.answered = now, false
	q.updateJobLocked(e)
}

func (q *Queue) storedLocked(e *entry) Stored {
	return Stored{ID: e.rowID, Job: e.job, ReadyAt: e.readyAt, StartedAt: e.startedAt, Answered: e.answered, Charged: e.charged}
}

// restoreJobsLocked re-queues the persisted non-auto jobs in their stored
// order (each gets a new seq, so per-MR FIFO holds across the restart and
// they stay ahead of jobs enqueued after it), applying the replay rule. The
// jobs finished within DoneTTL and the dropped rows are remembered for the
// event dedup (Restore).
func (q *Queue) restoreJobsLocked(ctx context.Context) {
	now := q.now()
	done, err := q.persist.ListDone(ctx, now.Add(-DoneTTL))
	if err != nil {
		q.log.Warn("queue: load finished jobs", "err", err)
	}
	q.seenJ = append(q.seenJ, done...)
	list, err := q.persist.ListJobs(ctx)
	if err != nil {
		q.log.Warn("queue: load persisted jobs", "err", err)
		return
	}
	counts := map[string]int{}
	for _, s := range list {
		j := s.Job
		log := q.log.With("mr", j.MRKey(), "kind", j.Kind, "project", j.ProjectPath, "note", j.NoteID, "enqueued_at", j.EnqueuedAt)
		drop := func(result, msg string) {
			if err := q.persist.FinishJob(ctx, s.ID, now); err != nil {
				log.Warn("queue: delete persisted job", "err", err)
			}
			if result != RestoreInvalid {
				q.seenJ = append(q.seenJ, j) // its event was handled (answered) or is too old
				if q.onDrop != nil {
					q.onDrop(j, result)
				}
			}
			log.Warn(msg)
			q.observeRestore(j.Kind, result)
			counts[result]++
		}
		switch {
		case !restorable(j):
			drop(RestoreInvalid, "queue: unusable persisted job dropped")
			continue
		case q.maxAge > 0 && !j.EnqueuedAt.IsZero() && now.Sub(j.EnqueuedAt) > q.maxAge:
			drop(RestoreExpired, "queue: persisted job older than queue.manual_max_age dropped")
			continue
		case !s.StartedAt.IsZero() && s.Answered:
			drop(RestoreAnswered, "queue: interrupted job had already answered in its discussion, dropped instead of replayed")
			continue
		}
		if j.EnqueuedAt.IsZero() {
			j.EnqueuedAt = now
		}
		e := &entry{job: j, key: j.MRKey(), readyAt: s.ReadyAt, seq: q.nextSeqLocked(),
			rowID: s.ID, charged: s.Charged, admitted: s.Charged}
		if e.readyAt.IsZero() {
			e.readyAt = now
		}
		e.retry = e.readyAt.After(now)
		result := RestoreQueued
		if !s.StartedAt.IsZero() {
			result = RestoreReplayed
			log.Info("queue: interrupted job replayed", "started_at", s.StartedAt, "charged", s.Charged)
		}
		q.pending = append(q.pending, e)
		q.restoredJ = append(q.restoredJ, j)
		q.observeRestore(j.Kind, result)
		counts[result]++
	}
	if len(list) > 0 {
		q.log.Info("queue: restored persisted jobs", "queued", counts[RestoreQueued], "replayed", counts[RestoreReplayed],
			"expired", counts[RestoreExpired], "answered", counts[RestoreAnswered], "invalid", counts[RestoreInvalid])
	}
}

// restorable reports whether a persisted row is a usable job: a kind that
// is not an automatic review, a project and an MR — or, for a pipeline job,
// its pipeline instead of the MR (the main branch's pipeline, or a branch
// pipeline the handler fans out to the branch's MRs; core.JobPipeline).
func restorable(j core.Job) bool {
	if j.Kind == "" || j.Kind == core.JobAutoReview || j.ProjectID == 0 {
		return false
	}
	return j.MRIID != 0 || (j.Kind == core.JobPipeline && j.Pipeline != nil)
}

// ---------------------------------------------------------------------------
// The running job in the handler's context
// ---------------------------------------------------------------------------

// runState is the running job (core.RunState), put into the handler's
// context by the queue and used by the reviewer (Admitted; MarkAnswered at
// a job's final reply) and by WrapLimiter below.
type runState struct {
	q   *Queue
	e   *entry
	job core.Job // copy taken at start: e.job is not touched while e runs
}

var _ core.RunState = (*runState)(nil)

func withRun(ctx context.Context, q *Queue, e *entry) context.Context {
	// Typed as the interface: go-arch-lint's deepScan would otherwise read
	// the injection as core depending on queue.
	var s core.RunState = &runState{q: q, e: e, job: e.job}
	return core.WithRunState(ctx, s)
}

func (s *runState) Job() core.Job { return s.job }

func (s *runState) MarkAnswered() {
	s.q.mu.Lock()
	defer s.q.mu.Unlock()
	if s.e.rowID != 0 && !s.e.answered {
		s.e.answered = true
		s.q.updateJobLocked(s.e)
	}
}

func (s *runState) MarkCharged() {
	s.q.mu.Lock()
	defer s.q.mu.Unlock()
	if !s.e.charged {
		s.e.charged = true
		s.q.updateJobLocked(s.e)
	}
}

func (s *runState) Admitted() bool {
	s.q.mu.Lock()
	defer s.q.mu.Unlock()
	return s.e.admitted
}

// ---------------------------------------------------------------------------
// WrapLimiter: was the user charged, and do not charge them twice
// ---------------------------------------------------------------------------

// WrapLimiter returns l aware of the running queue job (core.RunState in
// the context):
//   - a committed booking or a recorded run with a user (the per-user
//     limits count it) marks the job charged (RunState.MarkCharged);
//   - a job whose earlier attempt was charged was already admitted and
//     counted against its user's limits (RunState.Admitted: it resumes after
//     a RetryLater, in process or after a restart, or is replayed after an
//     interruption): its bookings skip the per-user limits and the MR rerun
//     cooldown (the global limit and a per-kind cap still apply) and its
//     runs are booked without the user and MR, so the per-user windows count
//     the command once while the global window counts every real Claude
//     run. The reviewer skips the access check of such an attempt too.
//
// Outside a queue job every call passes through unchanged.
func WrapLimiter(l core.Limiter) core.Limiter {
	if l == nil {
		return nil
	}
	return &chargeLimiter{Limiter: l}
}

type chargeLimiter struct{ core.Limiter }

func (l *chargeLimiter) Reserve(ctx context.Context, req core.ReserveRequest) (core.Decision, core.Reservation, error) {
	s := core.RunStateFrom(ctx)
	if s == nil {
		return l.Limiter.Reserve(ctx, req)
	}
	user := req.Usage.UserID != 0
	if user && s.Admitted() {
		req.User = nil
		req.Usage.UserID, req.Usage.MRKey = 0, ""
	}
	dec, res, err := l.Limiter.Reserve(ctx, req)
	if res != nil && user {
		res = &chargedReservation{Reservation: res, s: s}
	}
	return dec, res, err
}

func (l *chargeLimiter) Record(ctx context.Context, u core.Usage) error {
	s := core.RunStateFrom(ctx)
	if s == nil || u.UserID == 0 {
		return l.Limiter.Record(ctx, u)
	}
	if s.Admitted() {
		u.UserID, u.MRKey = 0, ""
	}
	err := l.Limiter.Record(ctx, u)
	if err == nil {
		s.MarkCharged()
	}
	return err
}

type chargedReservation struct {
	core.Reservation
	s core.RunState
}

func (r *chargedReservation) Commit(ctx context.Context) (bool, error) {
	ok, err := r.Reservation.Commit(ctx)
	if ok && err == nil {
		r.s.MarkCharged()
	}
	return ok, err
}
