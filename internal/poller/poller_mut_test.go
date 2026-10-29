package poller

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
)

// sortedSource returns the fake's MRs ordered by IID (deterministic order).
type sortedSource struct{ *fakeGitLab }

func (s sortedSource) ListUpdatedMRs(ctx context.Context, group string, since time.Time) ([]gitlab.MRSummary, error) {
	out, err := s.fakeGitLab.ListUpdatedMRs(ctx, group, since)
	sort.Slice(out, func(i, j int) bool { return out[i].IID < out[j].IID })
	return out, err
}

// A failing MR neither stops the other MRs of the tick nor lets the cursor
// move past it, even when a later MR succeeded long after it (outside the
// overlap): its event is picked up once the error is gone.
func TestMutFailedMRDoesNotBlockOthersNorGetLost(t *testing.T) {
	h := newHarness(t)
	h.p.o.Source = sortedSource{h.gl}
	h.mr(1)
	h.mr(2)
	h.tick() // baseline

	h.note(1, "d1", true, 500, "alice", "@reviewbot review")
	h.touch(1, func(m *gitlab.MRSummary) {})
	h.now = h.now.Add(10 * time.Minute)
	h.touch(2, func(m *gitlab.MRSummary) { m.SHA = "sha2" })
	h.gl.discErr[[2]int64{7, 1}] = errors.New("502")
	js := h.tick()
	wantKinds(t, js, core.JobAutoReview) // MR 2 still processed
	if js[0].MRIID != 2 {
		t.Fatalf("job %+v", js[0])
	}
	if c := h.st.cursors[""]; c.After(h.gl.mrs[[2]int64{7, 1}].UpdatedAt) {
		t.Fatalf("cursor %v moved past the failed MR (updated %v)", c, h.gl.mrs[[2]int64{7, 1}].UpdatedAt)
	}

	delete(h.gl.discErr, [2]int64{7, 1})
	h.now = h.now.Add(10 * time.Minute)
	js = h.tick()
	wantKinds(t, js, core.JobReview)
	if js[0].MRIID != 1 || js[0].NoteID != 500 {
		t.Fatalf("job %+v", js[0])
	}
	// MR 2 was done: not replayed.
	wantKinds(t, h.tick())
}

// The cursor follows the newest processed updated_at, and Tick reports the
// number of jobs it enqueued.
func TestMutCursorAdvancesAndTickCounts(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.tick()
	h.now = h.now.Add(5 * time.Minute)
	h.touch(1, func(m *gitlab.MRSummary) { m.SHA = "s1" })
	h.note(1, "d1", true, 10, "alice", "@reviewbot пауза")
	h.now = h.now.Add(30 * time.Second)
	h.jobs = nil
	n, err := h.p.Tick(context.Background())
	if err != nil || n != 2 || len(h.jobs) != 2 {
		t.Fatalf("tick n=%d err=%v jobs=%v", n, err, kinds(h.jobs))
	}
	if c, want := h.st.cursors[""], h.gl.mrs[[2]int64{7, 1}].UpdatedAt; !c.Equal(want) {
		t.Fatalf("cursor %v, want the MR's updated_at %v", c, want)
	}
}

// Observe gets every tick's job count and first error.
func TestMutObserveCalled(t *testing.T) {
	h := newHarness(t)
	var calls, jobs int
	h.p.o.Observe = func(d time.Duration, n int, err error) { calls++; jobs += n }
	h.mr(1)
	h.tick()
	h.touch(1, func(m *gitlab.MRSummary) { m.SHA = "x" })
	h.tick()
	if calls != 2 || jobs != 1 {
		t.Fatalf("observe calls=%d jobs=%d", calls, jobs)
	}
}

// Notes of an MR the poller did not know, that existed long before (an MR
// moved into the group or reopened), are history; a new note is not.
func TestMutUnknownOldMRNotesAreHistory(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.tick() // baseline
	h.now = h.now.Add(time.Hour)

	old := h.now.Add(-48 * time.Hour)
	k := [2]int64{7, 9}
	h.gl.mrs[k] = &gitlab.MRSummary{ProjectID: 7, ProjectPath: "team/api", IID: 9, State: "opened", SHA: "s9",
		Title: "feat: moved (PROJ-1)", CreatedAt: old, UpdatedAt: h.now.Add(time.Second)}
	h.gl.disc[k] = []core.Discussion{{ID: "old", Individual: true, Notes: []core.Note{
		{ID: 5, AuthorID: 1, AuthorUsername: "alice", Body: "@reviewbot review", CreatedAt: old.Add(time.Hour)},
	}}}
	h.note(9, "new", true, 6, "bob", "@reviewbot пауза")
	js := h.tick()
	wantKinds(t, js, core.JobAutoReview, core.JobPause)
	if js[1].NoteID != 6 {
		t.Fatalf("note job %+v", js[1])
	}
}

// An unknown MR that is not open (merged/closed) gets no review.
func TestMutUnknownClosedMRNoReview(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.tick()
	h.touch(3, func(m *gitlab.MRSummary) { m.State = "merged" })
	wantKinds(t, h.tick())
}

// The highest note ID is remembered even when discussions are not in ID
// order, and a new reply in an old thread (after an old note) is found.
func TestMutNoteOrderAndRepliesInOldThreads(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.tick()
	h.note(1, "a", true, 50, "alice", "@reviewbot пауза")
	h.note(1, "b", true, 40, "bob", "просто комментарий")
	h.touch(1, func(m *gitlab.MRSummary) {})
	wantKinds(t, h.tick(), core.JobPause)
	if got := h.st.mrs[[2]int64{7, 1}].LastNoteID; got != 50 {
		t.Fatalf("LastNoteID=%d, want 50", got)
	}
	h.touch(1, func(m *gitlab.MRSummary) {})
	wantKinds(t, h.tick()) // nothing replayed

	// Reply in thread "b" (its first note is old).
	h.note(1, "b", true, 60, "carol", "@reviewbot что это?")
	h.touch(1, func(m *gitlab.MRSummary) {})
	js := h.tick()
	wantKinds(t, js, core.JobChat)
	if js[0].NoteID != 60 {
		t.Fatalf("job %+v", js[0])
	}
}

// failingState fails the cursor read.
type failingState struct {
	*memState
	err error
}

func (f failingState) GetPollCursor(context.Context, string) (time.Time, bool, error) {
	return time.Time{}, false, f.err
}

// A store error reading the cursor is an error, not a reason to re-baseline
// (which would move the cursor to now and silently drop events).
func TestMutCursorReadErrorDoesNotRebaseline(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.tick()
	before := h.st.cursors[""]
	h.p.o.State = failingState{h.st, errors.New("db locked")}
	h.now = h.now.Add(time.Hour)
	if _, err := h.p.Tick(context.Background()); err == nil {
		t.Fatal("cursor read error swallowed")
	}
	if !h.st.cursors[""].Equal(before) {
		t.Fatalf("cursor rewritten to %v", h.st.cursors[""])
	}
}

// errListSource fails the MR listing.
type errListSource struct{ *fakeGitLab }

func (errListSource) ListUpdatedMRs(context.Context, string, time.Time) ([]gitlab.MRSummary, error) {
	return nil, errors.New("503")
}

// A failed baseline query records no cursor, so the baseline is retried.
func TestMutBootstrapErrorKeepsNoCursor(t *testing.T) {
	h := newHarness(t)
	h.p.o.Source = errListSource{h.gl}
	if _, err := h.p.Tick(context.Background()); err == nil {
		t.Fatal("bootstrap error swallowed")
	}
	if _, ok := h.st.cursors[""]; ok {
		t.Fatal("cursor recorded after a failed baseline")
	}
}

// Run polls immediately, then stops with the context.
func TestMutRunTicksImmediately(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	ticked := make(chan struct{})
	h.p.o.Observe = func(time.Duration, int, error) { close(ticked) }
	h.p.o.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.p.Run(ctx); close(done) }()
	select {
	case <-ticked:
	case <-time.After(2 * time.Second):
		t.Fatal("no immediate tick")
	}
	cancel()
	<-done
}

// Dedup keys: an auto review without a head and a non-note job carrying a
// head SHA have no stable identity and are never dropped.
func TestMutDedupKeyOnlyForIdentifiableJobs(t *testing.T) {
	if k := Key(core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 2}); k != "" {
		t.Errorf("auto without head: key %q", k)
	}
	if k := Key(core.Job{Kind: core.JobRefreshTitle, ProjectID: 1, MRIID: 2, HeadSHA: "a"}); k != "" {
		t.Errorf("title refresh with head: key %q", k)
	}
	d := NewDedup(time.Hour, nil)
	j := core.Job{Kind: core.JobChat, NoteID: 1}
	if !d.First(j) || d.First(j) {
		t.Error("default clock dedup broken")
	}
}
