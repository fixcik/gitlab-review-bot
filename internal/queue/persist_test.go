package queue

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// memPersister is an in-memory Persister shared by "restarted" queues.
type memPersister struct {
	mu     sync.Mutex
	rows   map[string]Pending
	jobs   map[int64]Stored
	lastID int64
	done   []doneJob
}

type doneJob struct {
	job core.Job
	at  time.Time
}

func newMemPersister() *memPersister {
	return &memPersister{rows: map[string]Pending{}, jobs: map[int64]Stored{}}
}

func (m *memPersister) AddJob(_ context.Context, s Stored) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastID++
	s.ID = m.lastID
	m.jobs[s.ID] = s
	return s.ID, nil
}
func (m *memPersister) UpdateJob(_ context.Context, s Stored) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[s.ID]; ok {
		m.jobs[s.ID] = s
	}
	return nil
}
func (m *memPersister) FinishJob(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.jobs[id]; ok {
		m.done = append(m.done, doneJob{job: s.Job, at: at})
	}
	delete(m.jobs, id)
	kept := m.done[:0]
	for _, d := range m.done {
		if !d.at.Before(at.Add(-DoneTTL)) {
			kept = append(kept, d)
		}
	}
	m.done = kept
	return nil
}
func (m *memPersister) ListDone(_ context.Context, since time.Time) ([]core.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.Job
	for _, d := range m.done {
		if !d.at.Before(since) {
			out = append(out, d.job)
		}
	}
	return out, nil
}
func (m *memPersister) ListJobs(context.Context) ([]Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Stored, 0, len(m.jobs))
	for _, s := range m.jobs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// jobList returns the stored non-auto jobs by ID.
func (m *memPersister) jobList() []Stored { l, _ := m.ListJobs(context.Background()); return l }

func (m *memPersister) SavePending(_ context.Context, p Pending) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[p.Job.MRKey()] = p
	return nil
}
func (m *memPersister) DeletePending(_ context.Context, pid, iid int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, core.MRKey(pid, iid))
	return nil
}
func (m *memPersister) ListPending(context.Context) ([]Pending, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Pending
	for _, p := range m.rows {
		out = append(out, p)
	}
	return out, nil
}
func (m *memPersister) get(mr int64) (Pending, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[core.MRKey(1, mr)]
	return p, ok
}
func (m *memPersister) len() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.rows) }

// A postponed (RetryLater) auto review survives a restart: the next queue
// reloads it and runs it at the saved time; once done its row is removed.
func TestPersistedRetrySurvivesRestart(t *testing.T) {
	p := newMemPersister()
	q1 := New(Options{Concurrency: 1, Persist: p})
	retryAt := time.Now().Add(time.Hour) // far away: q1 never retries it
	ran := make(chan struct{}, 1)
	stop1 := start(t, q1, func(ctx context.Context, j core.Job) error {
		ran <- struct{}{}
		return &core.RetryLater{At: retryAt, Reason: "limit"}
	})
	j := auto(5, "abc")
	j.ForceFull = true
	j.ProjectPath = "g/api"
	q1.Enqueue(j)
	<-ran
	waitFor(t, time.Second, func() bool { r, ok := p.get(5); return ok && r.ReadyAt.Equal(retryAt) })
	stop1() // restart: the in-memory retry is dropped...
	if _, ok := p.get(5); !ok {
		t.Fatal("persisted retry deleted on shutdown")
	}

	// ...and restored by the next queue (ready time moved up for the test).
	r, _ := p.get(5)
	r.ReadyAt = time.Now()
	p.SavePending(context.Background(), r)
	q2 := New(Options{Concurrency: 1, Persist: p})
	var rec recorder
	start(t, q2, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 1 })
	got := rec.get()[0]
	if got.Kind != core.JobAutoReview || got.MRIID != 5 || got.HeadSHA != "abc" || !got.ForceFull || got.ProjectPath != "g/api" {
		t.Fatalf("restored job %+v", got)
	}
	waitFor(t, time.Second, func() bool { return p.len() == 0 })
}

// A debounced auto job not yet run is persisted and reloaded (one row per
// MR); a manual job gets its own row in the job table. A newer auto job
// enqueued before Run wins over the restored one (dedupe per MR) and
// inherits ForceFull; the manual job is restored too.
func TestPersistedDebouncedAndDedupe(t *testing.T) {
	p := newMemPersister()
	q1 := New(Options{Concurrency: 1, Debounce: time.Hour, Persist: p}) // never runs: the process "stops"
	first := auto(1, "a")
	first.ForceFull = true
	q1.Enqueue(first)
	q1.Enqueue(auto(1, "b")) // debounced replacement
	q1.Enqueue(auto(2, "x"))
	q1.Enqueue(manual(3, 10))
	if p.len() != 2 {
		t.Fatalf("auto rows %d, want 2", p.len())
	}
	if r, _ := p.get(1); r.Job.HeadSHA != "b" || !r.Job.ForceFull {
		t.Fatalf("debounced row %+v", r)
	}
	if _, ok := p.get(3); ok {
		t.Fatal("manual job stored as a pending auto review")
	}
	if jl := p.jobList(); len(jl) != 1 || jl[0].Job.MRIID != 3 || jl[0].Job.NoteID != 10 || !jl[0].StartedAt.IsZero() {
		t.Fatalf("manual rows %+v", jl)
	}

	for _, mr := range []int64{1, 2} {
		r, _ := p.get(mr)
		r.ReadyAt = time.Now()
		p.SavePending(context.Background(), r)
	}
	q2 := New(Options{Concurrency: 1, Persist: p})
	q2.Enqueue(auto(1, "c")) // newer, before Run
	var rec recorder
	start(t, q2, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 3 })
	byMR := map[int64]core.Job{}
	for _, j := range rec.get() {
		if _, dup := byMR[j.MRIID]; dup {
			t.Fatalf("MR %d ran twice", j.MRIID)
		}
		byMR[j.MRIID] = j
	}
	if byMR[1].HeadSHA != "c" || !byMR[1].ForceFull || byMR[2].HeadSHA != "x" || byMR[3].Kind != core.JobReview {
		t.Fatalf("jobs %+v", byMR)
	}
	waitFor(t, time.Second, func() bool { return p.len() == 0 && len(p.jobList()) == 0 })
}

// A job interrupted by shutdown keeps its row; a failed (non-retry) job is
// removed.
func TestPersistedInterruptedKeptFailedRemoved(t *testing.T) {
	p := newMemPersister()
	q := New(Options{Concurrency: 2, Persist: p})
	block := make(chan struct{})
	stop := start(t, q, func(ctx context.Context, j core.Job) error {
		if j.MRIID == 1 {
			return errors.New("boom")
		}
		close(block)
		<-ctx.Done()
		return ctx.Err()
	})
	q.Enqueue(auto(1, "a"))
	q.Enqueue(auto(2, "b"))
	<-block
	waitFor(t, time.Second, func() bool { _, ok := p.get(1); return !ok })
	stop()
	if _, ok := p.get(2); !ok {
		t.Fatal("interrupted job must stay persisted")
	}
}
