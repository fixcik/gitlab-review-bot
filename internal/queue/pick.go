package queue

import (
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

type entry struct {
	job     core.Job
	key     string
	auto    bool
	readyAt time.Time
	seq     uint64
	retry   bool // waiting for its *core.RetryLater time (see Stats)

	// Persisted state of a job that is not an automatic review (see
	// Stored); rowID 0 = not persisted.
	rowID     int64
	startedAt time.Time
	answered  bool
	charged   bool
	// admitted: an earlier attempt charged the job's user (restored with
	// Charged set, or charged and then postponed by RetryLater in this
	// process): core.RunState.Admitted, WrapLimiter relaxes the user limits.
	admitted bool
}

// better reports whether a should be dispatched before b (both ready).
func better(a, b *entry) bool {
	if a.auto != b.auto {
		return a.auto
	}
	if !a.readyAt.Equal(b.readyAt) {
		return a.readyAt.Before(b.readyAt)
	}
	return a.seq < b.seq
}

// pickLocked removes and returns the best ready job whose MR is idle, or nil.
// Ready manual jobs of one MR are considered strictly in FIFO order (seq).
// A manual job waiting for its retry time (*core.RetryLater: the
// refresh_status chain while a pipeline runs, a command postponed by a
// Claude limit) does not hold the MR's later jobs; once its time comes it
// is ready again and, keeping its seq, goes before the jobs enqueued after
// it.
func (q *Queue) pickLocked(now time.Time) *entry {
	headManual := make(map[string]*entry)
	for _, e := range q.pending {
		if e.auto || e.readyAt.After(now) {
			continue
		}
		if h, ok := headManual[e.key]; !ok || e.seq < h.seq {
			headManual[e.key] = e
		}
	}
	var (
		best    *entry
		bestIdx = -1
	)
	for i, e := range q.pending {
		if q.running[e.key] || e.readyAt.After(now) {
			continue
		}
		if !e.auto && headManual[e.key] != e {
			continue
		}
		if best == nil || better(e, best) {
			best, bestIdx = e, i
		}
	}
	if best == nil {
		return nil
	}
	q.pending = append(q.pending[:bestIdx], q.pending[bestIdx+1:]...)
	if best.auto && q.autoPending[best.key] == best {
		delete(q.autoPending, best.key)
	}
	return best
}

// nextWakeLocked returns the earliest future ready time among pending jobs
// whose MR is idle (zero if none). Jobs blocked by a running MR or by the
// concurrency limit are woken by worker completion instead.
func (q *Queue) nextWakeLocked(now time.Time) time.Time {
	var next time.Time
	for _, e := range q.pending {
		if q.running[e.key] || !e.readyAt.After(now) {
			continue
		}
		if next.IsZero() || e.readyAt.Before(next) {
			next = e.readyAt
		}
	}
	return next
}
