package queue

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/limits"
)

// nonAutoKinds is every kind the job table persists.
func nonAutoKinds() []core.JobKind {
	var out []core.JobKind
	for _, k := range core.AllJobKinds() {
		if k != core.JobAutoReview {
			out = append(out, k)
		}
	}
	return out
}

func cmd(kind core.JobKind, mr, note int64) core.Job {
	return core.Job{Kind: kind, ProjectID: 1, ProjectPath: "g/p", MRIID: mr, NoteID: note, UserID: 7, Username: "dev",
		DiscussionID: "d" + string(rune('a'+note%26)), Question: "q"}
}

// restoreLog records OnRestore calls.
type restoreLog struct {
	mu  sync.Mutex
	got map[core.JobKind][]string
}

func (r *restoreLog) on(kind core.JobKind, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.got == nil {
		r.got = map[core.JobKind][]string{}
	}
	r.got[kind] = append(r.got[kind], result)
}

func (r *restoreLog) results(kind core.JobKind) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got[kind]...)
}

// Every non-auto kind survives a restart while queued: restored in enqueue
// order per MR, ahead of a job enqueued after the restart, and its row is
// deleted once it ran.
func TestRestoreEveryManualKindInOrder(t *testing.T) {
	p := newMemPersister()
	q1 := New(Options{Concurrency: 1, Persist: p}) // never runs: the process stops with all of them queued
	kinds := nonAutoKinds()
	for i, k := range kinds {
		j := cmd(k, int64(1+i%2), int64(100+i))
		if k == core.JobPipeline {
			j.Pipeline = &core.PipelineRef{ID: 9, Status: "failed"}
		}
		q1.Enqueue(j)
	}
	if n := len(p.jobList()); n != len(kinds) {
		t.Fatalf("rows %d, want %d", n, len(kinds))
	}

	var rl restoreLog
	q2 := New(Options{Concurrency: 2, Persist: p, OnRestore: rl.on})
	restored := q2.Restore()
	if len(restored) != len(kinds) {
		t.Fatalf("Restore returned %d jobs, want %d", len(restored), len(kinds))
	}
	late := cmd(core.JobChat, 1, 999) // after the restart: must run after MR 1's restored jobs
	q2.Enqueue(late)
	var rec recorder
	start(t, q2, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	waitFor(t, time.Second, func() bool { return len(rec.get()) == len(kinds)+1 })

	last := map[int64]int64{}
	seen := map[core.JobKind]bool{}
	for _, j := range rec.get() {
		if j.NoteID <= last[j.MRIID] {
			t.Fatalf("per-MR FIFO violated for MR %d: %+v", j.MRIID, rec.get())
		}
		last[j.MRIID] = j.NoteID
		seen[j.Kind] = true
		if j.NoteID != 999 && (j.Username != "dev" || j.UserID != 7 || j.Question != "q" || j.DiscussionID == "") {
			t.Fatalf("restored job lost fields: %+v", j)
		}
		if j.Kind == core.JobPipeline && (j.Pipeline == nil || j.Pipeline.ID != 9) {
			t.Fatalf("pipeline lost: %+v", j)
		}
	}
	for _, k := range kinds {
		if !seen[k] {
			t.Errorf("kind %s not restored", k)
		}
		if got := rl.results(k); len(got) != 1 || got[0] != RestoreQueued {
			t.Errorf("OnRestore(%s) = %v, want [queued]", k, got)
		}
	}
	waitFor(t, time.Second, func() bool { return len(p.jobList()) == 0 })
}

// Jobs older than ManualMaxAge are dropped on restore (row deleted,
// OnRestore expired); younger ones run.
func TestRestoreDropsExpiredManualJobs(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	p := newMemPersister()
	q1 := New(Options{Concurrency: 1, Persist: p, Now: clock})
	q1.Enqueue(cmd(core.JobChat, 1, 1))
	mu.Lock()
	now = now.Add(5 * time.Hour)
	mu.Unlock()
	q1.Enqueue(cmd(core.JobReview, 2, 2))
	mu.Lock()
	now = now.Add(2 * time.Hour) // first: 7h old, second: 2h old
	mu.Unlock()

	var rl restoreLog
	q2 := New(Options{Concurrency: 1, Persist: p, Now: clock, ManualMaxAge: 6 * time.Hour, OnRestore: rl.on})
	var rec recorder
	start(t, q2, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := rec.get(); len(got) != 1 || got[0].NoteID != 2 {
		t.Fatalf("ran %+v, want only the 2h old job", got)
	}
	if got := rl.results(core.JobChat); len(got) != 1 || got[0] != RestoreExpired {
		t.Fatalf("chat restore %v", got)
	}
	waitFor(t, time.Second, func() bool { return len(p.jobList()) == 0 })
}

// Interrupted jobs: replayed unless their final reply was out during the
// interrupted attempt (core.RunState.MarkAnswered, called by the reviewer's
// env.Reply). Other replies of the job (a "fixed" reply in a finding
// thread), a job without a discussion (refresh, pipeline) and a reply
// before a RetryLater (the handler wants another attempt) do not count.
func TestInterruptedReplayRules(t *testing.T) {
	p := newMemPersister()
	answered := cmd(core.JobChat, 1, 1)   // its final reply is out, then it is interrupted
	silent := cmd(core.JobReview, 2, 2)   // interrupted before replying
	elsewhere := cmd(core.JobCheck, 3, 3) // only a per-finding reply (not its final one) is out
	refresh := core.Job{Kind: core.JobRefreshStatus, ProjectID: 1, MRIID: 4}
	topLevel := cmd(core.JobHelp, 5, 5) // no discussion: the reply is a note addressing the author
	topLevel.DiscussionID = ""
	postponed := cmd(core.JobChat, 6, 6) // "⏳ postponed" reply + RetryLater: queued again
	running := make(chan struct{}, 8)
	q1 := New(Options{Concurrency: 8, Persist: p})
	stop := start(t, q1, func(ctx context.Context, j core.Job) error {
		switch j.MRIID {
		case 1, 5:
			core.RunStateFrom(ctx).MarkAnswered()
		case 6:
			core.RunStateFrom(ctx).MarkAnswered()
			running <- struct{}{}
			return &core.RetryLater{At: time.Now().Add(time.Hour), Reason: "claude limit"}
		}
		running <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	for _, j := range []core.Job{answered, silent, elsewhere, refresh, topLevel, postponed} {
		q1.Enqueue(j)
	}
	for range 6 {
		<-running
	}
	waitFor(t, time.Second, func() bool { return q1.Stats().RetryWait == 1 })
	stop() // grace 0: the running ones are cancelled and keep their rows

	byMR := map[int64]Stored{}
	for _, s := range p.jobList() {
		byMR[s.Job.MRIID] = s
	}
	if len(byMR) != 6 {
		t.Fatalf("rows %+v", byMR)
	}
	for mr, want := range map[int64]bool{1: true, 2: false, 3: false, 4: false, 5: true, 6: false} {
		if byMR[mr].Answered != want {
			t.Errorf("MR %d answered=%v, want %v", mr, byMR[mr].Answered, want)
		}
	}
	if !byMR[6].StartedAt.IsZero() || byMR[2].StartedAt.IsZero() {
		t.Fatalf("started_at: postponed %v, interrupted %v", byMR[6].StartedAt, byMR[2].StartedAt)
	}

	var rl restoreLog
	q2 := New(Options{Concurrency: 8, Persist: p, OnRestore: rl.on})
	var rec recorder
	start(t, q2, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 3 })
	time.Sleep(20 * time.Millisecond)
	ran := map[int64]bool{}
	for _, j := range rec.get() {
		ran[j.MRIID] = true
	}
	if !ran[2] || !ran[3] || !ran[4] || ran[1] || ran[5] || ran[6] {
		t.Fatalf("replayed MRs %v; want 2, 3, 4 (6 waits for its retry time)", ran)
	}
	for kind, want := range map[core.JobKind]string{core.JobReview: RestoreReplayed, core.JobCheck: RestoreReplayed,
		core.JobRefreshStatus: RestoreReplayed, core.JobHelp: RestoreAnswered} {
		if got := rl.results(kind); len(got) != 1 || got[0] != want {
			t.Errorf("OnRestore(%s) = %v, want %s", kind, got, want)
		}
	}
	if got := rl.results(core.JobChat); len(got) != 2 {
		t.Errorf("chat restores %v, want answered + queued", got)
	}
	if s := q2.Stats(); s.RetryWait != 1 {
		t.Fatalf("postponed chat not waiting for its retry: %+v", s)
	}
}

// fakeLimiter records what reached the real limiter.
type fakeLimiter struct {
	core.Limiter
	mu       sync.Mutex
	reserves []core.ReserveRequest
	records  []core.Usage
}

func (f *fakeLimiter) Reserve(_ context.Context, req core.ReserveRequest) (core.Decision, core.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserves = append(f.reserves, req)
	return core.Decision{Allowed: true}, &fakeRes{f: f, u: req.Usage}, nil
}

func (f *fakeLimiter) Record(_ context.Context, u core.Usage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, u)
	return nil
}

type fakeRes struct {
	f    *fakeLimiter
	u    core.Usage
	done bool
}

func (r *fakeRes) Commit(ctx context.Context) (bool, error) {
	if r.done {
		return false, nil
	}
	r.done = true
	return true, r.f.Record(ctx, r.u)
}
func (r *fakeRes) Release() { r.done = true }

// A job interrupted after its Claude run was booked against the user is
// replayed without the per-user limits and its runs are booked without the
// user (counted once per user, every run globally); a job interrupted
// before any run goes through the user limits again.
func TestReplayChargedJobSkipsUserLimits(t *testing.T) {
	p := newMemPersister()
	lim := &fakeLimiter{}
	l := WrapLimiter(lim)
	userLim := &core.UserLimits{RerunsPerHour: 5, MRRerunCooldown: 10 * time.Minute}
	book := func(ctx context.Context, j core.Job) error {
		_, res, err := l.Reserve(ctx, core.ReserveRequest{Usage: core.Usage{UserID: j.UserID, Kind: core.UsageRerun, MRKey: j.MRKey()},
			Username: j.Username, User: userLim, Global: true})
		if err != nil {
			return err
		}
		if j.MRIID == 1 { // the run starts: the booking is committed
			if _, err := res.Commit(ctx); err != nil {
				return err
			}
			return l.Record(ctx, core.Usage{UserID: j.UserID, Kind: core.UsageRerun, MRKey: j.MRKey()}) // a second run
		}
		res.Release()
		return nil
	}
	running := make(chan struct{}, 2)
	q1 := New(Options{Concurrency: 2, Persist: p})
	stop := start(t, q1, func(ctx context.Context, j core.Job) error {
		if err := book(ctx, j); err != nil {
			return err
		}
		running <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	q1.Enqueue(cmd(core.JobReview, 1, 1)) // charged
	q1.Enqueue(cmd(core.JobReview, 2, 2)) // not charged
	<-running
	<-running
	stop()
	for _, s := range p.jobList() {
		if s.Charged != (s.Job.MRIID == 1) {
			t.Fatalf("row %+v: charged=%v", s.Job, s.Charged)
		}
	}

	lim.mu.Lock()
	lim.reserves, lim.records = nil, nil
	lim.mu.Unlock()
	q2 := New(Options{Concurrency: 2, Persist: p})
	var rec recorder
	start(t, q2, func(ctx context.Context, j core.Job) error { rec.add(j); return book(ctx, j) })
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 2 })
	waitFor(t, time.Second, func() bool { return len(p.jobList()) == 0 })

	lim.mu.Lock()
	reserves, records := lim.reserves, lim.records
	lim.mu.Unlock()
	for _, r := range reserves {
		charged := r.Usage.MRKey == "" // MR 1, rewritten
		if charged && (r.User != nil || r.Usage.UserID != 0 || !r.Global) {
			t.Errorf("charged replay booked with user limits: %+v", r)
		}
		if !charged && (r.User == nil || r.Usage.UserID != 7) {
			t.Errorf("uncharged replay skipped the user limits: %+v", r)
		}
	}
	if len(reserves) != 2 {
		t.Fatalf("reserves %+v", reserves)
	}
	for _, u := range records {
		if u.UserID != 0 || u.MRKey != "" {
			t.Errorf("charged replay recorded a run against the user: %+v", u)
		}
	}
	if len(records) != 2 {
		t.Fatalf("records %+v, want both runs of the replay (global)", records)
	}

	// Outside a queue job the wrapper passes everything through.
	if _, _, err := l.Reserve(context.Background(), core.ReserveRequest{Usage: core.Usage{UserID: 3}, User: userLim}); err != nil {
		t.Fatal(err)
	}
	lim.mu.Lock()
	defer lim.mu.Unlock()
	if r := lim.reserves[len(lim.reserves)-1]; r.User == nil || r.Usage.UserID != 3 {
		t.Fatalf("pass-through changed %+v", r)
	}
}

// startCtx is start with the run context handed back, so a test can end
// Run and observe the drain.
func startCtx(t *testing.T, q *Queue, h core.Handler) (cancel func(), done <-chan struct{}) {
	t.Helper()
	ctx, c := context.WithCancel(context.Background())
	d := make(chan struct{})
	go func() {
		if err := q.Run(ctx, h); err != nil {
			t.Errorf("Run: %v", err)
		}
		close(d)
	}()
	t.Cleanup(func() {
		c()
		q.Abort()
		<-d
	})
	return c, d
}

// Drain: a running job finishes within the grace with its context intact;
// no pending job starts; a job enqueued during the drain is persisted.
func TestDrainFinishesWithinGrace(t *testing.T) {
	p := newMemPersister()
	q := New(Options{Concurrency: 1, Persist: p, ShutdownGrace: 5 * time.Second})
	started, finish := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var ran []int64
	var ctxErr error
	cancel, done := startCtx(t, q, func(ctx context.Context, j core.Job) error {
		mu.Lock()
		ran = append(ran, j.MRIID)
		mu.Unlock()
		if j.MRIID == 1 {
			close(started)
			<-finish
			mu.Lock()
			ctxErr = ctx.Err()
			mu.Unlock()
		}
		return nil
	})
	q.Enqueue(cmd(core.JobChat, 1, 1))
	<-started
	q.Enqueue(cmd(core.JobReview, 2, 2)) // waits for the only worker
	cancel()
	waitFor(t, time.Second, func() bool { return q.Stats().Draining })
	q.Enqueue(cmd(core.JobHelp, 3, 3)) // a webhook during the drain
	q.Enqueue(auto(4, "a"))
	close(finish)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not end when the running job finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if ctxErr != nil {
		t.Fatalf("running job's context cancelled during the grace: %v", ctxErr)
	}
	if len(ran) != 1 {
		t.Fatalf("jobs started during the drain: %v", ran)
	}
	rows := map[int64]bool{}
	for _, s := range p.jobList() {
		rows[s.Job.MRIID] = true
	}
	if rows[1] || !rows[2] || !rows[3] {
		t.Fatalf("rows after drain %v: want the finished job gone, the queued and the drain-time job kept", rows)
	}
	if _, ok := p.get(4); !ok {
		t.Fatal("auto job enqueued during the drain not persisted")
	}
	if q.Stats().Draining {
		t.Fatal("still draining after Run returned")
	}
}

// Drain: past the grace the running job is cancelled and keeps its row
// (manual and auto); Abort ends the wait early.
func TestDrainCancelsAfterGraceAndKeepsRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grace time.Duration
		abort bool
	}{{"grace", 50 * time.Millisecond, false}, {"abort", time.Hour, true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newMemPersister()
			q := New(Options{Concurrency: 2, Persist: p, ShutdownGrace: tc.grace})
			running := make(chan struct{}, 2)
			cancel, done := startCtx(t, q, func(ctx context.Context, j core.Job) error {
				running <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			})
			q.Enqueue(cmd(core.JobChat, 1, 1))
			q.Enqueue(auto(2, "a"))
			<-running
			<-running
			began := time.Now()
			cancel()
			if tc.abort {
				time.Sleep(20 * time.Millisecond)
				q.Abort()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("drain did not cancel the running jobs")
			}
			if d := time.Since(began); !tc.abort && d < tc.grace {
				t.Fatalf("cancelled after %v, before the grace %v", d, tc.grace)
			}
			if jl := p.jobList(); len(jl) != 1 || jl[0].StartedAt.IsZero() {
				t.Fatalf("interrupted manual row %+v", jl)
			}
			if _, ok := p.get(2); !ok {
				t.Fatal("interrupted auto row deleted")
			}
		})
	}
}

// Grace 0 keeps the old behaviour: the handler's context is cancelled at
// once.
func TestDrainZeroGraceCancelsAtOnce(t *testing.T) {
	q := New(Options{Concurrency: 1})
	running := make(chan struct{})
	cancel, done := startCtx(t, q, func(ctx context.Context, j core.Job) error {
		close(running)
		<-ctx.Done()
		return errors.New("stopped")
	})
	q.Enqueue(cmd(core.JobChat, 1, 1))
	<-running
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("grace 0 did not cancel at once")
	}
}

// A job postponed by RetryLater in this process after its attempt charged
// the user resumes admitted, like a charged job restored after a restart:
// the run state says so (the reviewer skips the access check) and its next
// booking skips the user limits; a job postponed before any charge is not
// admitted. Admitted stays set for every later attempt.
func TestInProcessRetryAfterChargeIsAdmitted(t *testing.T) {
	lim := &fakeLimiter{}
	l := WrapLimiter(lim)
	userLim := &core.UserLimits{RerunsPerHour: 5}
	var mu sync.Mutex
	seen := map[int64][]bool{}
	q := New(Options{Concurrency: 2})
	start(t, q, func(ctx context.Context, j core.Job) error {
		s := core.RunStateFrom(ctx)
		if s == nil || s.Job().NoteID != j.NoteID {
			t.Errorf("no run state for %+v", j)
			return nil
		}
		mu.Lock()
		seen[j.MRIID] = append(seen[j.MRIID], core.RunAdmitted(ctx))
		n := len(seen[j.MRIID])
		mu.Unlock()
		_, res, err := l.Reserve(ctx, core.ReserveRequest{Usage: core.Usage{UserID: j.UserID, Kind: core.UsageRerun, MRKey: j.MRKey()},
			Username: j.Username, User: userLim, Global: true})
		if err != nil {
			return err
		}
		switch {
		case j.MRIID == 1 && n == 1: // charged, then a Claude usage limit
			if _, err := res.Commit(ctx); err != nil {
				return err
			}
			return &core.RetryLater{At: time.Now(), Reason: "limit"}
		case j.MRIID == 1 && n == 2: // resumed, postponed again (nothing new charged)
			res.Release()
			return &core.RetryLater{At: time.Now(), Reason: "limit"}
		case j.MRIID == 2 && n == 1: // postponed before any run
			res.Release()
			return &core.RetryLater{At: time.Now(), Reason: "global"}
		}
		res.Release()
		return nil
	})
	q.Enqueue(cmd(core.JobReview, 1, 1))
	q.Enqueue(cmd(core.JobReview, 2, 2))
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen[1]) == 3 && len(seen[2]) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	if got := seen[1]; got[0] || !got[1] || !got[2] {
		t.Errorf("charged job admitted per attempt %v, want [false true true]", got)
	}
	if got := seen[2]; got[0] || got[1] {
		t.Errorf("uncharged job admitted per attempt %v, want [false false]", got)
	}
	lim.mu.Lock()
	defer lim.mu.Unlock()
	for _, r := range lim.reserves {
		admitted := r.Usage.UserID == 0
		if admitted && (r.User != nil || r.Usage.MRKey != "" || !r.Global) {
			t.Errorf("admitted attempt booked with user limits: %+v", r)
		}
	}
	if n := len(lim.reserves); n != 5 {
		t.Fatalf("reserves %d, want 5", n)
	}
	if r := lim.reserves; r[0].Usage.UserID != 7 || r[0].User == nil {
		t.Fatalf("first attempt skipped the user limits: %+v", r[0])
	}
}

// usageMem is an in-memory core.Store of usage records (for limits.New).
type usageMem struct {
	mu   sync.Mutex
	recs []core.Usage
}

func (m *usageMem) GetMRState(context.Context, int64, int64) (*core.MRState, error) { return nil, nil }
func (m *usageMem) SaveMRState(context.Context, core.MRState) error                 { return nil }
func (m *usageMem) PruneUsage(context.Context, time.Time) error                     { return nil }
func (m *usageMem) Close() error                                                    { return nil }
func (m *usageMem) RecordUsage(_ context.Context, u core.Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, u)
	return nil
}
func (m *usageMem) ListUsage(_ context.Context, f core.UsageFilter) ([]core.Usage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.Usage
	for _, u := range m.recs {
		if (f.UserID == nil || *f.UserID == u.UserID) && (f.Kind == nil || *f.Kind == u.Kind) &&
			(f.MRKey == nil || *f.MRKey == u.MRKey) && !u.At.Before(f.Since) {
			out = append(out, u)
		}
	}
	return out, nil
}
func (m *usageMem) all() []core.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]core.Usage(nil), m.recs...)
}

// A learning job (thread_reply: a UsageLearn run booked with its author and
// review.learn.max_per_hour) interrupted after its run was charged is
// replayed admitted: the replay's booking drops the author, but the global
// limit and the per-kind cap still apply to it and count its run, so the
// cap holds across the restart (a later learning job sees both runs).
func TestReplayedLearnJobKeepsKindCap(t *testing.T) {
	p := newMemPersister()
	st := &usageMem{}
	now := time.Now()
	l := WrapLimiter(limits.New(st, 10, nil, func() time.Time { return now }))
	learn := func(ctx context.Context, j core.Job, perHour int) (bool, error) {
		dec, res, err := l.Reserve(ctx, core.ReserveRequest{Usage: core.Usage{UserID: j.UserID, Kind: core.UsageLearn, MRKey: j.MRKey()},
			Global: true, KindPerHour: perHour})
		if err != nil || !dec.Allowed {
			return false, err
		}
		_, err = res.Commit(ctx)
		return true, err
	}

	running := make(chan struct{}, 1)
	q1 := New(Options{Concurrency: 1, Persist: p})
	stop := start(t, q1, func(ctx context.Context, j core.Job) error {
		if ok, err := learn(ctx, j, 3); !ok || err != nil {
			t.Errorf("first attempt: %v %v", ok, err)
		}
		running <- struct{}{}
		<-ctx.Done() // interrupted after the run started
		return ctx.Err()
	})
	q1.Enqueue(cmd(core.JobThreadReply, 1, 1))
	<-running
	stop()
	if rows := p.jobList(); len(rows) != 1 || !rows[0].Charged {
		t.Fatalf("rows %+v", rows)
	}

	q2 := New(Options{Concurrency: 1, Persist: p})
	type result struct {
		mr       int64
		admitted bool
		ok       bool
	}
	got := make(chan result, 2)
	start(t, q2, func(ctx context.Context, j core.Job) error {
		ok, err := learn(ctx, j, 3)
		got <- result{j.MRIID, core.RunAdmitted(ctx), ok}
		return err
	})
	if r := <-got; r.mr != 1 || !r.admitted || !r.ok {
		t.Fatalf("replay %+v", r)
	}
	recs := st.all()
	if len(recs) != 2 || recs[0].UserID != 7 || recs[1].UserID != 0 || recs[1].MRKey != "" || recs[1].Kind != core.UsageLearn {
		t.Fatalf("records %+v: the replay's run must count once globally and for the cap, not against the author again", recs)
	}
	// The cap (3) counts the replay's run: one more learning run fits, the
	// next one does not.
	q2.Enqueue(cmd(core.JobThreadReply, 2, 2))
	if r := <-got; r.mr != 2 || r.admitted || !r.ok {
		t.Fatalf("third run %+v", r)
	}
	if ok, err := learn(context.Background(), cmd(core.JobThreadReply, 3, 3), 3); ok || err != nil {
		t.Fatalf("the cap let a fourth learning run through: %v %v", ok, err)
	}
}

// Pipeline jobs without an MR (the main branch's pipeline, "who broke
// main", and a branch pipeline the handler fans out to the branch's MRs)
// survive a restart like every other job; a pipeline job without an MR and
// without its pipeline is unusable.
func TestRestoreKeepsPipelineJobsWithoutMR(t *testing.T) {
	p := newMemPersister()
	q1 := New(Options{Concurrency: 1, Persist: p})
	mainPipe := core.Job{Kind: core.JobPipeline, ProjectID: 5, ProjectPath: "g/p", HeadSHA: "abc",
		Pipeline: &core.PipelineRef{ID: 9, Ref: "main", Status: "failed", Main: true}}
	branchPipe := core.Job{Kind: core.JobPipeline, ProjectID: 5, ProjectPath: "g/p",
		Pipeline: &core.PipelineRef{ID: 10, Ref: "feature", Status: "failed"}}
	q1.Enqueue(mainPipe)
	q1.Enqueue(branchPipe)
	if _, err := p.AddJob(context.Background(), Stored{Job: core.Job{Kind: core.JobPipeline, ProjectID: 5}}); err != nil {
		t.Fatal(err)
	}

	var rl restoreLog
	q2 := New(Options{Concurrency: 1, Persist: p, OnRestore: rl.on})
	got := q2.Restore()
	if len(got) != 2 || got[0].Pipeline == nil || got[0].Pipeline.ID != 9 || got[1].Pipeline == nil || got[1].Pipeline.ID != 10 {
		t.Fatalf("restored %+v, want both pipeline jobs", got)
	}
	if r := rl.results(core.JobPipeline); len(r) != 3 || r[0] != RestoreQueued || r[1] != RestoreQueued || r[2] != RestoreInvalid {
		t.Fatalf("OnRestore %v", r)
	}
	if n := len(p.jobList()); n != 2 {
		t.Fatalf("rows left %d, want 2 (the invalid one deleted)", n)
	}
}

// Restore returns every job whose event is already handled or queued, so
// the poller of events "both" does not enqueue it again after a restart:
// the jobs it queues again, the rows it drops (answered, expired) and the
// jobs that finished within DoneTTL before the restart (their rows are
// gone, a marker of each is kept). Older markers are forgotten.
func TestRestoreReturnsEveryHandledJob(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	nowFn := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	p := newMemPersister()

	// Process 1: note 1 finishes (long ago), note 2 finishes recently,
	// note 3 is interrupted after its answer, note 4 stays queued.
	q1 := New(Options{Concurrency: 1, Persist: p, Now: nowFn})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	finished := make(chan int64, 4)
	go func() {
		defer close(done)
		_ = q1.Run(ctx, func(ctx context.Context, j core.Job) error {
			if j.NoteID == 3 {
				core.RunStateFrom(ctx).MarkAnswered()
				stop()
				<-ctx.Done()
				return ctx.Err()
			}
			finished <- j.NoteID
			return nil
		})
	}()
	q1.Enqueue(cmd(core.JobChat, 1, 1))
	<-finished
	clock.Store(now.Add(DoneTTL - time.Minute).UnixNano()) // note 1's marker expires at the restart
	q1.Enqueue(cmd(core.JobChat, 2, 2))
	<-finished
	q1.Enqueue(cmd(core.JobChat, 3, 3))
	<-done
	q1.Enqueue(cmd(core.JobChat, 4, 4))

	clock.Store(now.Add(DoneTTL + time.Minute).UnixNano())
	var rl restoreLog
	q2 := New(Options{Concurrency: 1, Persist: p, Now: nowFn, OnRestore: rl.on})
	notes := map[int64]bool{}
	for _, j := range q2.Restore() {
		notes[j.NoteID] = true
	}
	if !notes[2] || !notes[3] || !notes[4] || notes[1] || len(notes) != 3 {
		t.Fatalf("restore returned notes %v, want 2 (finished), 3 (answered), 4 (queued)", notes)
	}
	if r := rl.results(core.JobChat); len(r) != 2 {
		t.Fatalf("OnRestore %v: finished jobs are not restore results", r)
	}
	if s := q2.Stats(); s.Pending != 1 {
		t.Fatalf("stats %+v, want only note 4 queued", s)
	}
	if d, _ := p.ListDone(context.Background(), time.Time{}); len(d) != 2 {
		t.Fatalf("markers %+v, want notes 2 and 3 (note 1 pruned)", d)
	}
}

// OnDrop hears about every job restore drops as answered or expired (main
// cleans up its note's reactions), not about invalid rows or queued jobs.
func TestRestoreOnDrop(t *testing.T) {
	now := time.Now()
	p := newMemPersister()
	ctx := context.Background()
	old := cmd(core.JobChat, 1, 1)
	old.EnqueuedAt = now.Add(-48 * time.Hour)
	answered := cmd(core.JobHelp, 2, 2)
	answered.EnqueuedAt = now
	queued := cmd(core.JobHelp, 3, 3)
	queued.EnqueuedAt = now
	for _, s := range []Stored{{Job: old}, {Job: answered, StartedAt: now, Answered: true}, {Job: queued}, {Job: core.Job{ProjectID: 1}}} {
		if _, err := p.AddJob(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	q := New(Options{Persist: p, ManualMaxAge: time.Hour, OnDrop: func(j core.Job, result string) {
		got = append(got, fmt.Sprintf("%d:%s", j.NoteID, result))
	}})
	q.Restore()
	if want := []string{"1:" + RestoreExpired, "2:" + RestoreAnswered}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OnDrop %v, want %v", got, want)
	}
}
