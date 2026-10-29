package queue

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// listPersister returns a fixed list from ListPending (in the given order)
// and accepts saves/deletes silently.
type listPersister struct{ list []Pending }

func (l *listPersister) SavePending(context.Context, Pending) error        { return nil }
func (l *listPersister) DeletePending(context.Context, int64, int64) error { return nil }
func (l *listPersister) ListPending(context.Context) ([]Pending, error)    { return l.list, nil }
func (l *listPersister) AddJob(context.Context, Stored) (int64, error)     { return 0, nil }
func (l *listPersister) UpdateJob(context.Context, Stored) error           { return nil }
func (l *listPersister) FinishJob(context.Context, int64, time.Time) error { return nil }
func (l *listPersister) ListJobs(context.Context) ([]Stored, error)        { return nil, nil }
func (l *listPersister) ListDone(context.Context, time.Time) ([]core.Job, error) {
	return nil, nil
}

// Among ready jobs of the same class the one that became ready first runs
// first, whatever order they were (re)loaded in.
func TestMutReadyTimeOrdersRestoredJobs(t *testing.T) {
	now := time.Now()
	p := &listPersister{list: []Pending{
		{Job: auto(2, "late"), ReadyAt: now.Add(-time.Minute)},
		{Job: auto(1, "early"), ReadyAt: now.Add(-5 * time.Minute)},
	}}
	q := New(Options{Concurrency: 1, Persist: p})
	var rec recorder
	start(t, q, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 2 })
	if got := rec.get(); got[0].MRIID != 1 || got[1].MRIID != 2 {
		t.Fatalf("order %d,%d; want the earlier ready job (MR 1) first", got[0].MRIID, got[1].MRIID)
	}
}

// A stray non-auto row does not stop the reload of the following rows.
func TestMutRestoreSkipsNonAutoRowsOnly(t *testing.T) {
	now := time.Now()
	p := &listPersister{list: []Pending{
		{Job: manual(1, 10), ReadyAt: now},
		{Job: auto(2, "a"), ReadyAt: now},
	}}
	q := New(Options{Concurrency: 1, Persist: p})
	var rec recorder
	start(t, q, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 1 })
	time.Sleep(20 * time.Millisecond)
	got := rec.get()
	if len(got) != 1 || got[0].MRIID != 2 || got[0].Kind != core.JobAutoReview {
		t.Fatalf("restored %+v; want only the auto job of MR 2", got)
	}
}

// The dispatcher wakes at the earliest future ready time, not at the ready
// time of whichever pending job happens to be last.
func TestMutWakesAtEarliestReadyTime(t *testing.T) {
	q := New(Options{Concurrency: 1, Debounce: 5 * time.Second})
	var calls atomic.Int32
	retried := make(chan time.Time, 1)
	rescheduled := make(chan struct{})
	start(t, q, func(ctx context.Context, j core.Job) error {
		if j.Kind != core.JobReview {
			return nil
		}
		if calls.Add(1) == 1 {
			defer close(rescheduled)
			return &core.RetryLater{At: time.Now().Add(80 * time.Millisecond)}
		}
		retried <- time.Now()
		return nil
	})
	begin := time.Now()
	q.Enqueue(manual(1, 1))
	<-rescheduled
	waitFor(t, time.Second, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.pending) == 1 })
	q.Enqueue(auto(2, "x")) // ready in 5s, appended after the retry
	select {
	case at := <-retried:
		if d := at.Sub(begin); d > 2*time.Second {
			t.Fatalf("retry ran after %v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retry of MR 1 did not run at its time (dispatcher slept until the later job)")
	}
}

func TestMutRunTwiceFails(t *testing.T) {
	q := New(Options{})
	start(t, q, func(ctx context.Context, j core.Job) error { return nil })
	waitFor(t, time.Second, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return q.started })
	if err := q.Run(context.Background(), func(ctx context.Context, j core.Job) error { return nil }); err == nil {
		t.Fatal("second concurrent Run must fail")
	}
}

// Enqueue stamps EnqueuedAt (used for queue-wait metrics) and keeps a
// caller-provided one.
func TestMutEnqueueStampsEnqueuedAt(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	q := New(Options{Concurrency: 1, Now: func() time.Time { return fixed }})
	var rec recorder
	start(t, q, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	q.Enqueue(manual(1, 1))
	own := manual(2, 2)
	own.EnqueuedAt = fixed.Add(-time.Hour)
	q.Enqueue(own)
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 2 })
	for _, j := range rec.get() {
		want := fixed
		if j.MRIID == 2 {
			want = own.EnqueuedAt
		}
		if !j.EnqueuedAt.Equal(want) {
			t.Fatalf("MR %d EnqueuedAt=%v want %v", j.MRIID, j.EnqueuedAt, want)
		}
	}
}

// Concurrency < 1 means one worker, not none.
func TestMutZeroConcurrencyStillRuns(t *testing.T) {
	q := New(Options{Concurrency: 0})
	var rec recorder
	start(t, q, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	q.Enqueue(manual(1, 1))
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 1 })
}

// A restored job keeps its original EnqueuedAt (queue-wait metrics span the
// restart); only a missing one is stamped.
func TestMutRestoreKeepsEnqueuedAt(t *testing.T) {
	orig := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	j1 := auto(1, "a")
	j1.EnqueuedAt = orig
	p := &listPersister{list: []Pending{
		{Job: j1, ReadyAt: time.Now()},
		{Job: auto(2, "b"), ReadyAt: time.Now()},
	}}
	q := New(Options{Concurrency: 1, Persist: p})
	var rec recorder
	start(t, q, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 2 })
	for _, j := range rec.get() {
		switch {
		case j.MRIID == 1 && !j.EnqueuedAt.Equal(orig):
			t.Fatalf("restored EnqueuedAt %v, want %v", j.EnqueuedAt, orig)
		case j.MRIID == 2 && j.EnqueuedAt.IsZero():
			t.Fatal("missing EnqueuedAt not stamped on restore")
		}
	}
}

// Only an auto job's retry is superseded by a newer pending auto job; a
// manual command postponed by RetryLater still runs even if an automatic
// review of the same MR is pending.
func TestMutManualRetryNotSupersededByPendingAuto(t *testing.T) {
	q := New(Options{Concurrency: 1, Debounce: time.Hour})
	var rec recorder
	release := make(chan struct{})
	var calls atomic.Int32
	start(t, q, func(ctx context.Context, j core.Job) error {
		rec.add(j)
		if j.Kind == core.JobReview && calls.Add(1) == 1 {
			<-release
			return &core.RetryLater{At: time.Now().Add(20 * time.Millisecond)}
		}
		return nil
	})
	q.Enqueue(manual(1, 1))
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 1 })
	q.Enqueue(auto(1, "a")) // pending (debounced) while the manual job runs
	close(release)
	waitFor(t, time.Second, func() bool { return calls.Load() == 2 })
}

// When a running auto job's retry is superseded by a newer auto job of the
// same MR that was blocked only by the running one, the newer job is
// dispatched right away (no timer covers a job of a busy MR).
func TestMutSupersededRetryWakesNewerJob(t *testing.T) {
	q := New(Options{Concurrency: 1})
	var rec recorder
	started := make(chan struct{})
	release := make(chan struct{})
	start(t, q, func(ctx context.Context, j core.Job) error {
		rec.add(j)
		if j.HeadSHA == "a" {
			close(started)
			<-release
			return &core.RetryLater{At: time.Now().Add(time.Hour)}
		}
		return nil
	})
	q.Enqueue(auto(1, "a"))
	<-started
	q.Enqueue(auto(1, "b"))
	time.Sleep(30 * time.Millisecond) // dispatcher sees "b" blocked by the running "a"
	close(release)
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 2 })
}

// ForceFull inherited from a superseded retry is persisted too, so a
// restart before the newer job runs does not downgrade it to incremental.
func TestMutSupersededRetryPersistsInheritedForceFull(t *testing.T) {
	p := newMemPersister()
	a := auto(1, "a") // restored, due now: runs at once
	a.ForceFull = true
	p.SavePending(context.Background(), Pending{Job: a, ReadyAt: time.Now()})
	q := New(Options{Concurrency: 1, Debounce: time.Hour, Persist: p})
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	start(t, q, func(ctx context.Context, j core.Job) error {
		defer close(done)
		close(started)
		<-release
		return &core.RetryLater{At: time.Now().Add(time.Hour)}
	})
	<-started
	q.Enqueue(auto(1, "b")) // pending (debounced) while "a" runs
	if r, _ := p.get(1); r.Job.HeadSHA != "b" || r.Job.ForceFull {
		t.Fatalf("precondition: row %+v", r)
	}
	close(release)
	<-done
	waitFor(t, time.Second, func() bool { r, _ := p.get(1); return r.Job.HeadSHA == "b" && r.Job.ForceFull })
}
