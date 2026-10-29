package poller

import (
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Expired keys are pruned once the memory holds more than 4096 keys (notes,
// heads and pipeline keys together); live keys survive the prune and still
// drop duplicates, a pipeline key keeps only its live entries.
func TestDedupPrunesExpiredKeys(t *testing.T) {
	t0 := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	now := t0
	d := NewDedup(time.Hour, func() time.Time { return now })
	note := func(id int64) core.Job { return core.Job{Kind: core.JobChat, NoteID: id} }
	pipe := func(id int64, at time.Time) core.Job {
		return core.Job{Kind: core.JobPipeline, ProjectID: 1, MRIID: 7, Pipeline: &core.PipelineRef{ID: id, Status: "failed", At: at}}
	}
	pAt := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)

	for id := int64(1); id <= 4094; id++ {
		if !d.First(note(id)) {
			t.Fatalf("note %d dropped", id)
		}
	}
	if !d.First(pipe(1, pAt)) || !d.First(pipe(2, pAt)) {
		t.Fatal("pipelines dropped")
	}
	// 4096 keys: no prune yet. Pipeline 1 fails again later (a second
	// entry under the same key).
	now = t0.Add(50 * time.Minute)
	if !d.First(pipe(1, pAt.Add(10*time.Minute))) {
		t.Fatal("pipeline 1 failing again dropped")
	}
	if len(d.seen) != 4094 || len(d.pipes) != 2 || len(d.pipes[Key(pipe(1, pAt))]) != 2 {
		t.Fatalf("before expiry: %d notes, %d pipeline keys", len(d.seen), len(d.pipes))
	}

	// Everything recorded at t0 has expired. At exactly 4096 keys nothing
	// is pruned...
	now = t0.Add(90 * time.Minute)
	if !d.First(note(10_001)) {
		t.Fatal("new note dropped")
	}
	if len(d.seen) != 4095 {
		t.Fatalf("pruned at 4096 keys: %d notes left", len(d.seen))
	}
	// ...above it, the expired keys go.
	if !d.First(note(10_002)) {
		t.Fatal("new note dropped")
	}
	if len(d.seen) != 2 {
		t.Fatalf("after prune: %d notes, want the 2 live ones", len(d.seen))
	}
	k1 := Key(pipe(1, pAt))
	if len(d.pipes) != 1 || len(d.pipes[k1]) != 1 || !d.pipes[k1][0].at.Equal(pAt.Add(10*time.Minute)) {
		t.Fatalf("after prune: pipes %+v", d.pipes)
	}
	// Live keys still deduplicate; expired ones pass again.
	if d.First(note(10_001)) {
		t.Fatal("live note key lost by the prune")
	}
	if d.First(pipe(1, pAt.Add(10*time.Minute+time.Second))) {
		t.Fatal("live pipeline entry lost by the prune")
	}
	if !d.First(note(1)) || !d.First(pipe(2, pAt)) {
		t.Fatal("expired keys must pass again")
	}
}

// A pipeline event with an unknown time and one with a known time are never
// the same event, in either order; two unknown times are.
func TestDedupPipelineKnownVsUnknownTime(t *testing.T) {
	at := time.Date(2026, 10, 24, 10, 5, 0, 0, time.UTC)
	for _, order := range [][2]time.Time{{{}, at}, {at, {}}} {
		d := NewDedup(time.Hour, nil)
		job := func(at time.Time) core.Job {
			return core.Job{Kind: core.JobPipeline, ProjectID: 1, Pipeline: &core.PipelineRef{ID: 9, Status: "failed", At: at}}
		}
		if !d.First(job(order[0])) {
			t.Fatal("first event dropped")
		}
		if !d.First(job(order[1])) {
			t.Fatalf("known and unknown time treated as one event (order %v)", order)
		}
		if d.First(job(order[0])) || d.First(job(order[1])) {
			t.Fatal("repeats passed")
		}
	}
}

// The boundaries are inclusive: a key exactly TTL old still drops a
// duplicate (one nanosecond later it does not), and two pipeline times
// exactly PipelineSlack apart, in either direction, are one event.
func TestDedupBoundaries(t *testing.T) {
	t0 := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	now := t0
	d := NewDedup(time.Hour, func() time.Time { return now })
	n := core.Job{Kind: core.JobChat, NoteID: 1}
	pipe := func(id int64, at time.Time) core.Job {
		return core.Job{Kind: core.JobPipeline, ProjectID: 1, Pipeline: &core.PipelineRef{ID: id, Status: "failed", At: at}}
	}
	d.First(n)
	d.First(pipe(1, t0))
	d.First(pipe(2, t0))
	now = t0.Add(time.Hour)
	if d.First(n) {
		t.Error("note key exactly TTL old passed")
	}
	if d.First(pipe(1, t0)) {
		t.Error("pipeline entry exactly TTL old passed")
	}
	if d.First(pipe(2, t0.Add(PipelineSlack))) || d.First(pipe(2, t0.Add(-PipelineSlack))) {
		t.Error("pipeline times exactly PipelineSlack apart passed")
	}
	if !d.First(pipe(2, t0.Add(PipelineSlack+time.Nanosecond))) || !d.First(pipe(3, t0)) {
		t.Error("distinct pipeline events dropped")
	}
	now = t0.Add(2*time.Hour + time.Nanosecond)
	if !d.First(n) || !d.First(pipe(1, t0)) {
		t.Error("expired keys dropped")
	}

	// The prune uses the same inclusive TTL.
	d = NewDedup(time.Hour, func() time.Time { return now })
	now = t0
	d.First(n)
	d.First(pipe(1, t0))
	d.pruneLocked(t0.Add(time.Hour))
	if len(d.seen) != 1 || len(d.pipes) != 1 {
		t.Errorf("prune dropped keys exactly TTL old: %d notes, %d pipelines", len(d.seen), len(d.pipes))
	}
	d.pruneLocked(t0.Add(time.Hour + time.Nanosecond))
	if len(d.seen) != 0 || len(d.pipes) != 0 {
		t.Errorf("prune kept expired keys: %d notes, %d pipelines", len(d.seen), len(d.pipes))
	}
}
