package queue

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// failPersister is a memPersister whose calls can fail; it counts the calls.
type failPersister struct {
	*memPersister
	mu                                    sync.Mutex
	addErr, updateErr, deleteErr, listErr error
	listDoneErr                           error
	saveErr, delPendErr, listPendErr      error
	calls                                 map[string]int
	extraPending                          []Pending // returned after the stored rows (duplicates)
}

func newFailPersister() *failPersister {
	return &failPersister{memPersister: newMemPersister(), calls: map[string]int{}}
}

func (f *failPersister) hit(name string, err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
	return err
}

func (f *failPersister) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *failPersister) AddJob(ctx context.Context, s Stored) (int64, error) {
	if err := f.hit("add", f.addErr); err != nil {
		return 0, err
	}
	return f.memPersister.AddJob(ctx, s)
}
func (f *failPersister) UpdateJob(ctx context.Context, s Stored) error {
	if err := f.hit("update", f.updateErr); err != nil {
		return err
	}
	return f.memPersister.UpdateJob(ctx, s)
}
func (f *failPersister) FinishJob(ctx context.Context, id int64, at time.Time) error {
	if err := f.hit("delete", f.deleteErr); err != nil {
		return err
	}
	return f.memPersister.FinishJob(ctx, id, at)
}
func (f *failPersister) ListDone(ctx context.Context, since time.Time) ([]core.Job, error) {
	if err := f.hit("listdone", f.listDoneErr); err != nil {
		return nil, err
	}
	return f.memPersister.ListDone(ctx, since)
}
func (f *failPersister) ListJobs(ctx context.Context) ([]Stored, error) {
	if err := f.hit("list", f.listErr); err != nil {
		return nil, err
	}
	return f.memPersister.ListJobs(ctx)
}
func (f *failPersister) SavePending(ctx context.Context, p Pending) error {
	if err := f.hit("save", f.saveErr); err != nil {
		return err
	}
	return f.memPersister.SavePending(ctx, p)
}
func (f *failPersister) DeletePending(ctx context.Context, pid, iid int64) error {
	if err := f.hit("delpend", f.delPendErr); err != nil {
		return err
	}
	return f.memPersister.DeletePending(ctx, pid, iid)
}
func (f *failPersister) ListPending(ctx context.Context) ([]Pending, error) {
	if err := f.hit("listpend", f.listPendErr); err != nil {
		return nil, err
	}
	l, err := f.memPersister.ListPending(ctx)
	return append(l, f.extraPending...), err
}

// logBuf captures a queue's log lines.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
func (l *logBuf) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// wantWarn fails unless a WARN line with msg was logged.
func (l *logBuf) wantWarn(t *testing.T, msg string) {
	t.Helper()
	for _, line := range strings.Split(l.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, `msg="`+msg+`"`) {
			return
		}
	}
	t.Errorf("no warning %q in log:\n%s", msg, l.String())
}

var errDB = errors.New("db down")

// Persistence is best effort: whatever call fails, the job still runs and
// the failure is logged.
func TestCovPersistFailuresDoNotLoseTheRun(t *testing.T) {
	type tc struct {
		name  string
		fail  func(*failPersister)
		job   core.Job
		warn  string
		check func(t *testing.T, p *failPersister)
	}
	for _, c := range []tc{{
		name: "add job", fail: func(p *failPersister) { p.addErr = errDB }, job: cmd(core.JobChat, 1, 1),
		warn: "queue: persist job",
		check: func(t *testing.T, p *failPersister) {
			// Not durable: no row, so nothing to update or delete.
			if p.count("update") != 0 || p.count("delete") != 0 || len(p.jobList()) != 0 {
				t.Fatalf("calls %v rows %+v", p.calls, p.jobList())
			}
		},
	}, {
		name: "update job", fail: func(p *failPersister) { p.updateErr = errDB }, job: cmd(core.JobChat, 1, 1),
		warn: "queue: update persisted job",
		check: func(t *testing.T, p *failPersister) {
			if p.count("update") == 0 || p.count("delete") != 1 || len(p.jobList()) != 0 {
				t.Fatalf("finished job not deleted after a failed start update: calls %v rows %+v", p.calls, p.jobList())
			}
		},
	}, {
		name: "delete job", fail: func(p *failPersister) { p.deleteErr = errDB }, job: cmd(core.JobChat, 1, 1),
		warn: "queue: delete persisted job",
		check: func(t *testing.T, p *failPersister) {
			// The row stays: the next process runs the job again (at
			// least once, never lost).
			if l := p.jobList(); len(l) != 1 || l[0].StartedAt.IsZero() {
				t.Fatalf("rows %+v", l)
			}
		},
	}, {
		name: "save pending", fail: func(p *failPersister) { p.saveErr = errDB }, job: auto(1, "a"),
		warn: "queue: persist pending job",
		check: func(t *testing.T, p *failPersister) {
			if p.len() != 0 || p.count("delpend") != 1 {
				t.Fatalf("calls %v rows %d", p.calls, p.len())
			}
		},
	}, {
		name: "delete pending", fail: func(p *failPersister) { p.delPendErr = errDB }, job: auto(1, "a"),
		warn: "queue: delete persisted job",
		check: func(t *testing.T, p *failPersister) {
			if _, ok := p.get(1); !ok {
				t.Fatal("row gone although its delete failed")
			}
		},
	}} {
		t.Run(c.name, func(t *testing.T) {
			p := newFailPersister()
			c.fail(p)
			var logs logBuf
			q := New(Options{Concurrency: 1, Persist: p, Logger: logs.logger()})
			var rec recorder
			start(t, q, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
			q.Enqueue(c.job)
			waitFor(t, 5*time.Second, func() bool {
				q.mu.Lock()
				defer q.mu.Unlock()
				return len(rec.get()) == 1 && q.nRunning == 0
			})
			logs.wantWarn(t, c.warn)
			c.check(t, p)
		})
	}
}

// A failed load of one table does not stop the restore of the other.
func TestCovRestoreListFailures(t *testing.T) {
	for _, failJobs := range []bool{true, false} {
		p := newFailPersister()
		_ = p.memPersister.SavePending(context.Background(), Pending{Job: auto(1, "a")})
		_, _ = p.memPersister.AddJob(context.Background(), Stored{Job: cmd(core.JobChat, 2, 1)})
		if failJobs {
			p.listErr = errDB
		} else {
			p.listPendErr = errDB
		}
		var logs logBuf
		q := New(Options{Persist: p, Logger: logs.logger()})
		got := q.Restore()
		logs.wantWarn(t, "queue: load persisted jobs")
		if len(got) != 1 {
			t.Fatalf("failJobs=%v: restored %+v, want the other table's job", failJobs, got)
		}
		if want := core.JobAutoReview; failJobs && got[0].Kind != want || !failJobs && got[0].Kind != core.JobChat {
			t.Fatalf("failJobs=%v: restored %+v", failJobs, got)
		}
		if s := q.Stats(); s.Pending != 1 {
			t.Fatalf("failJobs=%v: %+v", failJobs, s)
		}
	}
}

// Unusable job rows (no kind, an automatic review in the job table, no
// project, no MR) are dropped on restore and deleted, even when the delete
// fails (logged; they are dropped again next time), and never run.
func TestCovRestoreDropsInvalidRows(t *testing.T) {
	for _, delErr := range []error{nil, errDB} {
		p := newFailPersister()
		ctx := context.Background()
		valid := cmd(core.JobChat, 9, 1)
		bad := []core.Job{
			{ProjectID: 1, MRIID: 2},
			{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 3},
			{Kind: core.JobChat, MRIID: 4},
			{Kind: core.JobChat, ProjectID: 1},
		}
		for _, j := range append(bad, valid) {
			if _, err := p.memPersister.AddJob(ctx, Stored{Job: j}); err != nil {
				t.Fatal(err)
			}
		}
		p.deleteErr = delErr
		var (
			logs logBuf
			rl   restoreLog
		)
		q := New(Options{Persist: p, Logger: logs.logger(), OnRestore: rl.on})
		got := q.Restore()
		if len(got) != 1 || got[0].MRIID != 9 {
			t.Fatalf("restored %+v, want only the valid job", got)
		}
		if p.count("delete") != len(bad) {
			t.Fatalf("deletes %d, want %d", p.count("delete"), len(bad))
		}
		invalid := len(rl.results("")) + len(rl.results(core.JobAutoReview))
		for _, r := range rl.results(core.JobChat) {
			if r == RestoreInvalid {
				invalid++
			}
		}
		if invalid != len(bad) {
			t.Fatalf("OnRestore invalid %d, want %d: %+v", invalid, len(bad), rl.got)
		}
		logs.wantWarn(t, "queue: unusable persisted job dropped")
		wantLeft := 1
		if delErr != nil {
			logs.wantWarn(t, "queue: delete persisted job")
			wantLeft += len(bad)
		}
		if l := p.jobList(); len(l) != wantLeft {
			t.Fatalf("delErr=%v: rows left %d, want %d", delErr, len(l), wantLeft)
		}
		if s := q.Stats(); s.Pending != 1 {
			t.Fatalf("%+v", s)
		}
	}
}

// A restored job row without an enqueue time gets the restore time (and is
// not expired by ManualMaxAge); one without a ready time is ready at once,
// one with a future ready time waits for it as a retry.
func TestCovRestoreFillsMissingTimes(t *testing.T) {
	clk := newFakeClock()
	now := clk.Now()
	p := newMemPersister()
	ctx := context.Background()
	noTimes := cmd(core.JobChat, 1, 1)
	later := cmd(core.JobChat, 2, 2)
	later.EnqueuedAt = now.Add(-time.Minute)
	_, _ = p.AddJob(ctx, Stored{Job: noTimes})
	_, _ = p.AddJob(ctx, Stored{Job: later, ReadyAt: now.Add(time.Hour)})
	q := New(Options{Persist: p, Now: clk.Now, ManualMaxAge: time.Hour})
	got := q.Restore()
	if len(got) != 2 || !got[0].EnqueuedAt.Equal(now) || !got[1].EnqueuedAt.Equal(later.EnqueuedAt) {
		t.Fatalf("restored %+v", got)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) != 2 {
		t.Fatalf("pending %d", len(q.pending))
	}
	a, b := q.pending[0], q.pending[1]
	if !a.readyAt.Equal(now) || a.retry || !a.job.EnqueuedAt.Equal(now) {
		t.Fatalf("job without times: ready %v retry %v enqueued %v", a.readyAt, a.retry, a.job.EnqueuedAt)
	}
	if !b.readyAt.Equal(now.Add(time.Hour)) || !b.retry {
		t.Fatalf("job with a future ready time: ready %v retry %v", b.readyAt, b.retry)
	}
}

// Two persisted auto rows of one MR (a Persister should keep one; the
// queue does not trust it) restore as one job: the first wins, a ForceFull
// of a later one is merged into it and saved (once; a duplicate without
// ForceFull changes nothing), and the rows after the duplicates still
// restore.
func TestCovRestoreMergesDuplicateAutoRows(t *testing.T) {
	p := newFailPersister()
	ctx := context.Background()
	clk := newFakeClock()
	first := auto(1, "a")
	_ = p.memPersister.SavePending(ctx, Pending{Job: first, ReadyAt: clk.Now()})
	dup := auto(1, "b")
	dup.ForceFull = true
	p.extraPending = []Pending{{Job: auto(1, "c")}, {Job: dup, ReadyAt: clk.Now().Add(time.Minute)}, {Job: auto(2, "z")}}
	var rl restoreLog
	q := New(Options{Persist: p, Now: clk.Now, OnRestore: rl.on})
	got := q.Restore()
	if len(got) != 2 || got[0].HeadSHA != "a" || got[1].HeadSHA != "z" {
		t.Fatalf("restored %+v, want the first row of MR 1 and MR 2", got)
	}
	if r := rl.results(core.JobAutoReview); len(r) != 2 {
		t.Fatalf("OnRestore %v, want two queued", r)
	}
	q.mu.Lock()
	n, e := len(q.pending), q.autoPending[first.MRKey()]
	q.mu.Unlock()
	if n != 2 || e == nil || !e.job.ForceFull || e.job.HeadSHA != "a" {
		t.Fatalf("pending %d, entry %+v", n, e)
	}
	if row, ok := p.get(1); !ok || !row.Job.ForceFull || row.Job.HeadSHA != "a" {
		t.Fatalf("merged ForceFull not saved: %+v", row)
	}
	if p.count("save") != 1 {
		t.Fatalf("saves %d, want 1 (a duplicate without ForceFull changes nothing)", p.count("save"))
	}
}

func TestCovNilWrappersAndHandler(t *testing.T) {
	if WrapLimiter(nil) != nil {
		t.Fatal("wrapping nil must stay nil (callers test for a missing client)")
	}
	q := New(Options{})
	if err := q.Run(context.Background(), nil); err == nil {
		t.Fatal("Run with a nil handler")
	}
	// A failed Run did not start the queue: a real one can.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Run(ctx, func(context.Context, core.Job) error { return nil }); err != nil {
		t.Fatalf("Run after the nil-handler error: %v", err)
	}
}

// A negative debounce is no debounce: an auto job is ready at once.
func TestCovNegativeDebounce(t *testing.T) {
	clk := newFakeClock()
	q := New(Options{Debounce: -time.Minute, Now: clk.Now})
	q.Enqueue(auto(1, "a"))
	q.mu.Lock()
	defer q.mu.Unlock()
	if e := q.pickLocked(clk.Now()); e == nil || e.job.HeadSHA != "a" {
		t.Fatalf("auto job not ready at once with a negative debounce: %+v", e)
	}
}

// runFor returns a context of a running queue job j with a persisted row
// (in-process, no Run): what WrapLimiter and the reviewer see in a handler.
func runFor(t *testing.T, j core.Job) (context.Context, *Queue, *entry, *memPersister) {
	t.Helper()
	p := newMemPersister()
	q := New(Options{Persist: p})
	id, _ := p.AddJob(context.Background(), Stored{Job: j})
	e := &entry{job: j, key: j.MRKey(), rowID: id}
	return withRun(context.Background(), q, e), q, e, p
}

// MarkAnswered writes the row once; a job without a row is only marked in
// memory.
func TestCovMarkAnswered(t *testing.T) {
	j := core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 2, NoteID: 3}
	ctx, _, e, p := runFor(t, j)
	p2 := newFailPersister()
	p2.memPersister = p
	e2 := &entry{job: j, key: j.MRKey()}
	s := core.RunStateFrom(ctx)
	s.MarkAnswered()
	s.MarkAnswered()
	if !e.answered || !p.jobList()[0].Answered {
		t.Fatalf("answered %v, row %+v", e.answered, p.jobList())
	}
	q := New(Options{Persist: p2})
	core.RunStateFrom(withRun(context.Background(), q, e2)).MarkAnswered()
	if e2.answered || p2.count("update") != 0 {
		t.Fatalf("a job without a row: answered %v, updates %d", e2.answered, p2.count("update"))
	}
}

// Record: outside a queue job, or for a run without a user, the call
// passes through unchanged and charges nobody; a user's run in a job
// charges it, and once admitted is recorded without the user and MR.
func TestCovChargeLimiterRecord(t *testing.T) {
	u := core.Usage{UserID: 7, Kind: core.UsageChat, MRKey: "1!2"}
	lim := &fakeLimiter{}
	l := WrapLimiter(lim)

	if err := l.Record(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	j := core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 2, NoteID: 3}
	ctx, _, e, p := runFor(t, j)
	noUser := core.Usage{Kind: core.UsageAuto, MRKey: "1!2"}
	if err := l.Record(ctx, noUser); err != nil {
		t.Fatal(err)
	}
	if e.charged {
		t.Fatal("a run without a user charged the job")
	}
	if err := l.Record(ctx, u); err != nil {
		t.Fatal(err)
	}
	if !e.charged || !p.jobList()[0].Charged {
		t.Fatal("a user's run did not charge the job")
	}
	e.admitted = true
	if err := l.Record(ctx, u); err != nil {
		t.Fatal(err)
	}
	want := []core.Usage{u, noUser, u, {Kind: core.UsageChat}}
	if len(lim.records) != len(want) {
		t.Fatalf("records %+v", lim.records)
	}
	for i := range want {
		if lim.records[i] != want[i] {
			t.Errorf("record %d: %+v, want %+v", i, lim.records[i], want[i])
		}
	}
}

// failLimiter fails Record.
type failLimiter struct{ core.Limiter }

func (failLimiter) Record(context.Context, core.Usage) error { return errDB }

// A run that could not be recorded did not charge the job.
func TestCovChargeLimiterRecordError(t *testing.T) {
	ctx, _, e, _ := runFor(t, core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 2, NoteID: 3})
	if err := WrapLimiter(failLimiter{}).Record(ctx, core.Usage{UserID: 7, Kind: core.UsageChat}); !errors.Is(err, errDB) {
		t.Fatalf("error not passed through: %v", err)
	}
	if e.charged {
		t.Fatal("charged by a failed record")
	}
}

// The happy path logs no warning: persisting, updating, deleting and
// restoring rows that all succeed (the warnings above are only for
// failures).
func TestCovPersistSuccessLogsNoWarning(t *testing.T) {
	p := newMemPersister()
	var logs logBuf
	q1 := New(Options{Concurrency: 2, Persist: p, Logger: logs.logger()})
	var rec recorder
	stop := start(t, q1, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	q1.Enqueue(auto(1, "a"))
	q1.Enqueue(cmd(core.JobChat, 2, 1))
	waitFor(t, 5*time.Second, func() bool {
		q1.mu.Lock()
		defer q1.mu.Unlock()
		return len(rec.get()) == 2 && q1.nRunning == 0
	})
	stop()
	// One row of each table left for the restart.
	_ = p.SavePending(context.Background(), Pending{Job: auto(3, "b")})
	_, _ = p.AddJob(context.Background(), Stored{Job: cmd(core.JobChat, 4, 2)})
	q2 := New(Options{Persist: p, Logger: logs.logger()})
	if got := q2.Restore(); len(got) != 3 || got[2].NoteID != 1 { // + the finished chat job (DoneTTL)
		t.Fatalf("restored %+v", got)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("warning on the happy path:\n%s", logs.String())
	}
}

// Finished jobs that cannot be listed only cost the dedup seed: restore
// logs it and still queues the persisted jobs.
func TestCovRestoreListDoneError(t *testing.T) {
	p := newFailPersister()
	if _, err := p.memPersister.AddJob(context.Background(), Stored{Job: cmd(core.JobChat, 1, 1)}); err != nil {
		t.Fatal(err)
	}
	p.listDoneErr = errDB
	var logs logBuf
	q := New(Options{Persist: p, Logger: logs.logger()})
	if got := q.Restore(); len(got) != 1 || got[0].NoteID != 1 {
		t.Fatalf("restored %+v", got)
	}
	logs.wantWarn(t, "queue: load finished jobs")
}
