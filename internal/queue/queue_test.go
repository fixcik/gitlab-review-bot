package queue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

type recorder struct {
	mu   sync.Mutex
	jobs []core.Job
}

func (r *recorder) add(j core.Job) { r.mu.Lock(); r.jobs = append(r.jobs, j); r.mu.Unlock() }
func (r *recorder) get() []core.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.Job(nil), r.jobs...)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func start(t *testing.T, q *Queue, h core.Handler) (cancel func()) {
	t.Helper()
	ctx, c := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		if err := q.Run(ctx, h); err != nil {
			t.Errorf("Run: %v", err)
		}
		close(done)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			c()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("Run did not return")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func auto(mr int64, sha string) core.Job {
	return core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: mr, HeadSHA: sha}
}

func manual(mr int64, note int64) core.Job {
	return core.Job{Kind: core.JobReview, ProjectID: 1, MRIID: mr, NoteID: note}
}

// fakeClock is a manual clock for Options.Now: time moves only on advance.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// settle waits until Run has dispatched everything that is ready at the
// (frozen) fake time and every handler returned: a probe job on its own MR
// is enqueued (ready at once) and awaited, then the queue must have nothing
// running. Run picks in one pass every ready job its concurrency allows,
// auto jobs first, so a job that was ready when the probe was picked has
// been dispatched by then.
func settle(t *testing.T, q *Queue, rec *recorder, probeNote int64) {
	t.Helper()
	q.Enqueue(manual(99, probeNote))
	waitFor(t, 5*time.Second, func() bool {
		for _, j := range rec.get() {
			if j.MRIID == 99 && j.NoteID == probeNote {
				return true
			}
		}
		return false
	})
	waitFor(t, 5*time.Second, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return q.nRunning == 0 })
}

// TestDebounceReplacesAndKeepsForceFull runs on a fake clock (the original
// slept on the wall clock: under CPU load a 15ms sleep could outlast the
// 40ms debounce, so "a" ran early and the test failed). Time only moves
// between steps, and settle syncs with Run, so every check is exact.
func TestDebounceReplacesAndKeepsForceFull(t *testing.T) {
	clk := newFakeClock()
	q := New(Options{Concurrency: 2, Debounce: 40 * time.Millisecond, Now: clk.Now})
	var rec recorder
	start(t, q, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	ran := func(mr int64) []core.Job {
		var out []core.Job
		for _, j := range rec.get() {
			if j.MRIID == mr {
				out = append(out, j)
			}
		}
		return out
	}

	first := auto(1, "a")
	first.ForceFull = true
	q.Enqueue(first) // t=0
	clk.advance(15 * time.Millisecond)
	q.Enqueue(auto(1, "b")) // t=15
	clk.advance(15 * time.Millisecond)
	q.Enqueue(auto(1, "c")) // t=30: MR 1 due at 70
	q.Enqueue(auto(2, "x")) // different MR, not replaced; due at 70

	clk.advance(25 * time.Millisecond) // t=55: 55ms after the first push, 25ms after the last
	settle(t, q, &rec, 1)
	if got := ran(1); len(got) != 0 {
		t.Fatalf("MR 1 ran before its debounce elapsed: %+v", got)
	}
	if got := ran(2); len(got) != 0 {
		t.Fatalf("MR 2 ran before its debounce elapsed: %+v", got)
	}

	clk.advance(14 * time.Millisecond) // t=69: still 1ms short
	settle(t, q, &rec, 2)
	if n := len(ran(1)) + len(ran(2)); n != 0 {
		t.Fatalf("auto jobs ran 1ms before their debounce: %+v", rec.get())
	}

	clk.advance(time.Millisecond) // t=70: both due
	settle(t, q, &rec, 3)
	clk.advance(time.Hour) // anything left over (a replaced "a"/"b") would be due now
	settle(t, q, &rec, 4)

	if got := ran(1); len(got) != 1 || got[0].HeadSHA != "c" || !got[0].ForceFull {
		t.Fatalf("MR 1: want one run of the latest sha with ForceFull, got %+v", got)
	}
	if got := ran(2); len(got) != 1 || got[0].HeadSHA != "x" || got[0].ForceFull {
		t.Fatalf("MR 2: want one run of x without ForceFull, got %+v", got)
	}
	if s := q.Stats(); s.Pending != 0 || s.Running != 0 {
		t.Fatalf("queue not empty: %+v", s)
	}
}

func TestManualNoDebounceAndPerMRSerialization(t *testing.T) {
	q := New(Options{Concurrency: 4, Debounce: time.Hour})
	var (
		active, maxActive atomic.Int32
		rec               recorder
	)
	start(t, q, func(ctx context.Context, j core.Job) error {
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		rec.add(j)
		active.Add(-1)
		return nil
	})
	for i := int64(1); i <= 4; i++ {
		q.Enqueue(manual(7, i))
	}
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 4 })
	if maxActive.Load() != 1 {
		t.Fatalf("same-MR jobs overlapped: max active %d", maxActive.Load())
	}
	for i, j := range rec.get() {
		if j.NoteID != int64(i+1) {
			t.Fatalf("FIFO violated: %+v", rec.get())
		}
	}
}

func TestAutoPriorityOverManual(t *testing.T) {
	q := New(Options{Concurrency: 1, Debounce: 0})
	var rec recorder
	release := make(chan struct{})
	start(t, q, func(ctx context.Context, j core.Job) error {
		if j.MRIID == 100 {
			<-release
		}
		rec.add(j)
		return nil
	})
	q.Enqueue(manual(100, 1)) // occupies the only slot
	waitFor(t, time.Second, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return q.nRunning == 1 })
	q.Enqueue(manual(2, 2))
	q.Enqueue(manual(3, 3))
	q.Enqueue(auto(4, "a"))
	time.Sleep(5 * time.Millisecond)
	close(release)
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 4 })
	jobs := rec.get()
	if jobs[1].MRIID != 4 || jobs[2].MRIID != 2 || jobs[3].MRIID != 3 {
		t.Fatalf("order: %v %v %v", jobs[1].MRIID, jobs[2].MRIID, jobs[3].MRIID)
	}
}

func TestRetryLaterReschedules(t *testing.T) {
	q := New(Options{Concurrency: 1})
	var (
		calls atomic.Int32
		times []time.Time
		mu    sync.Mutex
	)
	start(t, q, func(ctx context.Context, j core.Job) error {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		if calls.Add(1) == 1 {
			return &core.RetryLater{At: time.Now().Add(30 * time.Millisecond), Reason: "limit"}
		}
		return nil
	})
	q.Enqueue(manual(1, 1))
	waitFor(t, time.Second, func() bool { return calls.Load() == 2 })
	mu.Lock()
	defer mu.Unlock()
	if d := times[1].Sub(times[0]); d < 25*time.Millisecond {
		t.Fatalf("retried too early: %v", d)
	}
	time.Sleep(40 * time.Millisecond)
	if calls.Load() != 2 {
		t.Fatalf("extra calls: %d", calls.Load())
	}
}

func TestRetryLaterWrappedAndSupersededByNewerAuto(t *testing.T) {
	q := New(Options{Concurrency: 1, Debounce: 10 * time.Millisecond})
	var rec recorder
	firstStarted := make(chan struct{})
	proceed := make(chan struct{})
	start(t, q, func(ctx context.Context, j core.Job) error {
		rec.add(j)
		if j.HeadSHA == "a" {
			close(firstStarted)
			<-proceed
			return errors.Join(errors.New("wrapped"), &core.RetryLater{At: time.Now().Add(time.Hour)})
		}
		return nil
	})
	first := auto(1, "a")
	first.ForceFull = true
	q.Enqueue(first)
	<-firstStarted
	q.Enqueue(auto(1, "b")) // pending while "a" runs
	close(proceed)
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 2 })
	j := rec.get()[1]
	if j.HeadSHA != "b" || !j.ForceFull {
		t.Fatalf("want b with inherited ForceFull, got %+v", j)
	}
	q.mu.Lock()
	n := len(q.pending)
	q.mu.Unlock()
	if n != 0 {
		t.Fatalf("superseded retry still pending: %d", n)
	}
}

func TestNewAutoReplacesRetryWaitingJob(t *testing.T) {
	q := New(Options{Concurrency: 1, Debounce: 5 * time.Millisecond})
	var rec recorder
	start(t, q, func(ctx context.Context, j core.Job) error {
		rec.add(j)
		if j.HeadSHA == "a" {
			return &core.RetryLater{At: time.Now().Add(time.Hour)}
		}
		return nil
	})
	q.Enqueue(auto(1, "a"))
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 1 })
	waitFor(t, time.Second, func() bool { q.mu.Lock(); defer q.mu.Unlock(); return len(q.pending) == 1 })
	q.Enqueue(auto(1, "b"))
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 2 })
	if rec.get()[1].HeadSHA != "b" {
		t.Fatalf("got %+v", rec.get()[1])
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) != 0 {
		t.Fatalf("pending=%d", len(q.pending))
	}
}

func TestConcurrencyBoundAndPanicRecovery(t *testing.T) {
	q := New(Options{Concurrency: 2})
	var active, maxActive, done atomic.Int32
	start(t, q, func(ctx context.Context, j core.Job) error {
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		done.Add(1)
		if j.MRIID == 3 {
			panic("boom")
		}
		if j.MRIID == 4 {
			return errors.New("plain error")
		}
		return nil
	})
	for i := int64(1); i <= 6; i++ {
		q.Enqueue(manual(i, i))
	}
	waitFor(t, time.Second, func() bool { return done.Load() == 6 })
	if m := maxActive.Load(); m != 2 {
		t.Fatalf("max active %d, want 2", m)
	}
	// Queue still works after panic/error.
	q.Enqueue(manual(3, 99))
	waitFor(t, time.Second, func() bool { return done.Load() == 7 })
}

func TestGracefulShutdown(t *testing.T) {
	q := New(Options{Concurrency: 1})
	started := make(chan struct{})
	var finished atomic.Bool
	var calls atomic.Int32
	stop := start(t, q, func(ctx context.Context, j core.Job) error {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		finished.Store(true)
		return nil
	})
	q.Enqueue(manual(1, 1))
	<-started
	q.Enqueue(manual(2, 2)) // pending: never started
	q.Enqueue(auto(3, "a"))
	stop()
	if !finished.Load() {
		t.Fatal("Run returned before running handler finished")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	// Not run, but kept: a later auto job of MR 3 still debounces over it.
	q.Enqueue(auto(3, "b"))
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) != 2 || len(q.autoPending) != 1 || q.autoPending[core.MRKey(1, 3)].job.HeadSHA != "b" {
		t.Fatalf("pending after stop: %d, auto %d", len(q.pending), len(q.autoPending))
	}
}

func TestEnqueueBeforeRunAndConcurrently(t *testing.T) {
	q := New(Options{Concurrency: 3})
	q.Enqueue(manual(1, 1))
	var n atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); q.Enqueue(manual(int64(i%5+10), int64(i))) }()
	}
	start(t, q, func(ctx context.Context, j core.Job) error { n.Add(1); return nil })
	wg.Wait()
	waitFor(t, time.Second, func() bool { return n.Load() == 21 })
}

// A manual job waiting for its retry time (the refresh_status chain while
// the pipeline runs, a command postponed by a Claude limit) does not hold
// the MR's later jobs: they run now, in FIFO order and one at a time; the
// waiting job runs when its time comes, ahead of the jobs enqueued after it.
func TestRetryWaitingManualJobDoesNotBlockLaterJobs(t *testing.T) {
	now := time.Now()
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	q := New(Options{Concurrency: 4, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	var (
		rec      recorder
		attempts atomic.Int32
		inMR     atomic.Int32
		overlap  atomic.Bool
	)
	release := make(chan struct{})
	start(t, q, func(ctx context.Context, j core.Job) error {
		if inMR.Add(1) > 1 {
			overlap.Store(true)
		}
		defer inMR.Add(-1)
		rec.add(j)
		switch j.Kind {
		case core.JobRefreshStatus:
			if attempts.Add(1) == 1 {
				return &core.RetryLater{At: now.Add(time.Hour), Reason: "pipeline still running"}
			}
		case core.JobChat:
			if j.NoteID == 1 {
				<-release
			}
		}
		return nil
	})
	q.Enqueue(core.Job{Kind: core.JobRefreshStatus, ProjectID: 1, MRIID: 3})
	waitFor(t, time.Second, func() bool { return q.Stats().RetryWait == 1 })

	q.Enqueue(core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 3, NoteID: 1})
	q.Enqueue(core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 3, NoteID: 2})
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 2 })
	time.Sleep(20 * time.Millisecond)
	if n := len(rec.get()); n != 2 {
		t.Fatalf("note 2 ran while note 1 of the same MR runs: %+v", rec.get())
	}
	if s := q.Stats(); s.Pending != 1 || s.RetryWait != 1 || s.Running != 1 {
		t.Fatalf("stats %+v", s)
	}
	close(release)
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 3 })

	// The retry time comes while a later job is queued: the retried job
	// keeps its place ahead of it.
	q.mu.Lock()
	q.running["1!3"] = true // hold the MR so both are pending at once
	q.mu.Unlock()
	q.Enqueue(core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 3, NoteID: 4})
	clock.Store(now.Add(2 * time.Hour).UnixNano())
	q.mu.Lock()
	delete(q.running, "1!3")
	q.mu.Unlock()
	q.signal()
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 5 })

	got := rec.get()
	want := []int64{0, 1, 2, 0, 4}
	for i, j := range got {
		if j.NoteID != want[i] {
			t.Fatalf("order %+v, want notes %v", got, want)
		}
	}
	if got[3].Kind != core.JobRefreshStatus || overlap.Load() {
		t.Fatalf("order %+v overlap %v", got, overlap.Load())
	}
}
