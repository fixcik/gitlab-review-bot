package queue

import (
	"context"
	"log/slog"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// afterRunLocked handles the outcome of a finished handler: a
// *core.RetryLater re-schedules the job (or lets a newer auto job of the MR
// supersede it), anything else forgets the persisted row. It reports
// whether the job was retried or superseded, and whether shutdown
// interrupted it (the handler failed after its context was cancelled: the
// row is kept, the job runs again after the restart).
func (q *Queue) afterRunLocked(jobCtx context.Context, e *entry, err error, log *slog.Logger) (retried, interrupted bool) {
	interrupted = err != nil && jobCtx.Err() != nil
	if r, ok := core.AsRetryLater(err); ok && !interrupted {
		q.retryLocked(e, r, log)
		return true, false
	}
	if !e.auto {
		if !interrupted {
			q.finishJobLocked(e)
		}
		return false, interrupted
	}
	_, newer := q.autoPending[e.key]
	// Keep the row when a newer auto job of this MR replaced it (the row is
	// that job's now) or when shutdown interrupted this one (it runs again
	// after the restart); otherwise the job is done.
	if !newer && !interrupted {
		q.deleteLocked(e)
	}
	return false, interrupted
}

// retryLocked puts e back in the queue for r.At, unless a newer auto job of
// the same MR is pending: that one supersedes the retry.
func (q *Queue) retryLocked(e *entry, r *core.RetryLater, log *slog.Logger) {
	if e.auto {
		if newer, ok := q.autoPending[e.key]; ok {
			newer.job.ForceFull = newer.job.ForceFull || e.job.ForceFull
			q.saveLocked(newer)
			log.Info("queue: retry superseded by newer auto job", "reason", r.Reason)
			return
		}
	}
	e.readyAt = r.At
	e.retry = true
	if e.auto {
		e.seq = q.nextSeqLocked()
		q.autoPending[e.key] = e
		q.saveLocked(e)
	} else {
		// The handler asked to run the job again: the next attempt is a
		// fresh one (whatever it posted, e.g. "postponed", is expected).
		// An attempt that charged the user admitted the job: the next one
		// resumes it (core.RunState.Admitted), as after a restart.
		e.startedAt, e.answered = time.Time{}, false
		e.admitted = e.admitted || e.charged
		q.updateJobLocked(e)
	}
	// Manual jobs keep their seq: once At comes they go before the later
	// manual jobs of the same MR (FIFO); until then those run (pickLocked).
	q.pending = append(q.pending, e)
	log.Info("queue: job rescheduled", "at", r.At, "reason", r.Reason)
}
