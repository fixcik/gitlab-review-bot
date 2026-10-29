package poller

import (
	"fmt"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Dedup drops a job that was already enqueued from the other event source
// (gitlab.events: both): the webhook and the poller see the same push and
// the same comment. Keys live in memory for TTL.
type Dedup struct {
	mu   sync.Mutex
	ttl  time.Duration
	now  func() time.Time
	seen map[string]time.Time
	// pipes: pipeline events by Key, with the time each reached its status
	// (PipelineRef.At): the same pipeline failing again after a job retry
	// is a new event, the webhook and the poller seeing one failure are not.
	pipes map[string][]pipeSeen
}

type pipeSeen struct{ seen, at time.Time }

// PipelineSlack: two events of the same pipeline and status whose times
// differ by at most this much are one event. The Pipeline Hook carries
// finished_at (to the second), the API listing updated_at (set by the same
// transition); a retried job that makes the pipeline fail again takes longer
// than that.
const PipelineSlack = 5 * time.Second

// NewDedup returns a Dedup; now defaults to time.Now.
func NewDedup(ttl time.Duration, now func() time.Time) *Dedup {
	if now == nil {
		now = time.Now
	}
	return &Dedup{ttl: ttl, now: now, seen: map[string]time.Time{}, pipes: map[string][]pipeSeen{}}
}

// Key identifies the event behind a job: the note for comment jobs, the
// head commit for reviews, the pipeline and its status for pipeline jobs
// (plus the time it reached the status, compared with PipelineSlack). Jobs
// without a stable identity (a title-only refresh) get "" and are never
// dropped.
func Key(j core.Job) string {
	switch {
	case j.NoteID != 0:
		return fmt.Sprintf("note:%d", j.NoteID)
	case j.Kind == core.JobAutoReview && j.HeadSHA != "":
		return fmt.Sprintf("auto:%d:%d:%s:%t", j.ProjectID, j.MRIID, j.HeadSHA, j.ForceFull)
	case j.Kind == core.JobPipeline && j.Pipeline != nil && j.Pipeline.ID != 0:
		// The status is part of the key: retrying a job turns the same
		// pipeline from failed to success (and back).
		return fmt.Sprintf("pipeline:%d:%d:%d:%s", j.ProjectID, j.Pipeline.ID, j.MRIID, j.Pipeline.Status)
	}
	return ""
}

// First reports whether j is seen for the first time (and remembers it).
func (d *Dedup) First(j core.Job) bool {
	k := Key(j)
	if k == "" {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if len(d.seen)+len(d.pipes) > 4096 {
		d.pruneLocked(now)
	}
	if j.Kind == core.JobPipeline && j.Pipeline != nil {
		return d.firstPipeline(k, j.Pipeline.At, now)
	}
	if at, ok := d.seen[k]; ok && now.Sub(at) <= d.ttl {
		return false
	}
	d.seen[k] = now
	return true
}

// pruneLocked drops every expired key.
func (d *Dedup) pruneLocked(now time.Time) {
	for key, at := range d.seen {
		if now.Sub(at) > d.ttl {
			delete(d.seen, key)
		}
	}
	for key, es := range d.pipes {
		if es = d.live(es, now); len(es) == 0 {
			delete(d.pipes, key)
		} else {
			d.pipes[key] = es
		}
	}
}

// firstPipeline: a pipeline event is new unless one with the same key and
// a time within PipelineSlack was seen. Two unknown (zero) times are equal;
// an unknown and a known time are centuries apart, never the same event.
func (d *Dedup) firstPipeline(k string, at, now time.Time) bool {
	es := d.live(d.pipes[k], now)
	for _, e := range es {
		if diff := e.at.Sub(at); diff <= PipelineSlack && diff >= -PipelineSlack {
			d.pipes[k] = es
			return false
		}
	}
	d.pipes[k] = append(es, pipeSeen{seen: now, at: at})
	return true
}

// live drops the expired entries.
func (d *Dedup) live(es []pipeSeen, now time.Time) []pipeSeen {
	out := es[:0]
	for _, e := range es {
		if now.Sub(e.seen) <= d.ttl {
			out = append(out, e)
		}
	}
	return out
}

// Wrap returns an enqueue function that drops duplicates before enqueue.
func (d *Dedup) Wrap(enqueue func(core.Job)) func(core.Job) {
	return func(j core.Job) {
		if d.First(j) {
			enqueue(j)
		}
	}
}
