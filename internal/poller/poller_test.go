package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
)

// fakeGitLab keeps MRs and discussions; ListUpdatedMRs filters like GitLab.
type fakeGitLab struct {
	mrs      map[[2]int64]*gitlab.MRSummary
	disc     map[[2]int64][]core.Discussion
	discErr  map[[2]int64]error
	listCall []time.Time
	discRead int // ListDiscussions calls
}

func newFake() *fakeGitLab {
	return &fakeGitLab{mrs: map[[2]int64]*gitlab.MRSummary{}, disc: map[[2]int64][]core.Discussion{}, discErr: map[[2]int64]error{}}
}

func (f *fakeGitLab) ListUpdatedMRs(_ context.Context, group string, since time.Time) ([]gitlab.MRSummary, error) {
	f.listCall = append(f.listCall, since)
	var out []gitlab.MRSummary
	for _, m := range f.mrs {
		if since.IsZero() && m.State != "opened" {
			continue
		}
		if !since.IsZero() && m.UpdatedAt.Before(since) {
			continue
		}
		out = append(out, *m)
	}
	return out, nil
}

func (f *fakeGitLab) ListDiscussions(_ context.Context, projectID, mrIID int64) ([]core.Discussion, error) {
	f.discRead++
	k := [2]int64{projectID, mrIID}
	if err := f.discErr[k]; err != nil {
		return nil, err
	}
	return f.disc[k], nil
}

type memState struct {
	cursors map[string]time.Time
	mrs     map[[2]int64]events.PollMR
}

func newMem() *memState {
	return &memState{cursors: map[string]time.Time{}, mrs: map[[2]int64]events.PollMR{}}
}

func (m *memState) GetPollCursor(_ context.Context, s string) (time.Time, bool, error) {
	t, ok := m.cursors[s]
	return t, ok, nil
}
func (m *memState) SetPollCursor(_ context.Context, s string, t time.Time) error {
	m.cursors[s] = t
	return nil
}
func (m *memState) GetPollMR(_ context.Context, p, i int64) (*events.PollMR, error) {
	r, ok := m.mrs[[2]int64{p, i}]
	if !ok {
		return nil, nil
	}
	return &r, nil
}
func (m *memState) SavePollMR(_ context.Context, r events.PollMR) error {
	m.mrs[[2]int64{r.ProjectID, r.IID}] = r
	return nil
}

type harness struct {
	t    *testing.T
	gl   *fakeGitLab
	st   *memState
	p    *Poller
	now  time.Time
	jobs []core.Job
}

func newHarness(t *testing.T, groups ...string) *harness {
	h := &harness{t: t, gl: newFake(), st: newMem(), now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	h.p = New(Options{
		Source: h.gl, State: h.st, Groups: groups,
		Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", Groups: groups, LearnReplies: true}),
		Enqueue: func(j core.Job) { h.jobs = append(h.jobs, j) },
		Now:     func() time.Time { return h.now },
	})
	return h
}

// tick advances the clock and polls once, returning the jobs of this tick.
func (h *harness) tick() []core.Job {
	h.t.Helper()
	h.now = h.now.Add(30 * time.Second)
	h.jobs = nil
	if _, err := h.p.Tick(context.Background()); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
	return h.jobs
}

func (h *harness) mr(iid int64) *gitlab.MRSummary {
	k := [2]int64{7, iid}
	m, ok := h.gl.mrs[k]
	if !ok {
		m = &gitlab.MRSummary{ProjectID: 7, ProjectPath: "team/api", IID: iid, State: "opened", SHA: "sha0",
			Title: "feat: x (PROJ-1)", CreatedAt: h.now, UpdatedAt: h.now}
		h.gl.mrs[k] = m
	}
	return m
}

// touch marks an MR updated "now" (as GitLab does on any change).
func (h *harness) touch(iid int64, change func(m *gitlab.MRSummary)) {
	m := h.mr(iid)
	change(m)
	m.UpdatedAt = h.now.Add(time.Second)
}

func (h *harness) note(iid int64, disc string, individual bool, id int64, user, body string) {
	k := [2]int64{7, iid}
	n := core.Note{ID: id, AuthorID: id * 10, AuthorUsername: user, Body: body, CreatedAt: h.now.Add(time.Second)}
	for i := range h.gl.disc[k] {
		if h.gl.disc[k][i].ID == disc {
			h.gl.disc[k][i].Notes = append(h.gl.disc[k][i].Notes, n)
			return
		}
	}
	h.gl.disc[k] = append(h.gl.disc[k], core.Discussion{ID: disc, Individual: individual, Notes: []core.Note{n}})
}

func kinds(js []core.Job) []core.JobKind {
	var out []core.JobKind
	for _, j := range js {
		out = append(out, j.Kind)
	}
	return out
}

func wantKinds(t *testing.T, got []core.Job, want ...core.JobKind) {
	t.Helper()
	g := kinds(got)
	if len(g) != len(want) {
		t.Fatalf("jobs %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("jobs %v, want %v", g, want)
		}
	}
}

func TestBootstrapEnqueuesNothing(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.note(1, "d1", true, 100, "alice", "@reviewbot review") // history
	h.touch(2, func(m *gitlab.MRSummary) { m.Draft = true })
	wantKinds(t, h.tick())
	if _, ok := h.st.cursors[""]; !ok || len(h.st.mrs) != 2 {
		t.Fatalf("baseline not recorded: %+v %+v", h.st.cursors, h.st.mrs)
	}
	// Old notes stay history when the MR is touched later.
	h.touch(1, func(m *gitlab.MRSummary) {})
	wantKinds(t, h.tick())
}

func TestMREvents(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.tick() // baseline

	// New MR.
	h.touch(2, func(m *gitlab.MRSummary) { m.SHA = "n1" })
	js := h.tick()
	wantKinds(t, js, core.JobAutoReview)
	if js[0].MRIID != 2 || js[0].HeadSHA != "n1" || js[0].ForceFull || js[0].ProjectPath != "team/api" {
		t.Errorf("new MR job %+v", js[0])
	}
	// Seen again inside the overlap: nothing.
	wantKinds(t, h.tick())

	// Push.
	h.touch(1, func(m *gitlab.MRSummary) { m.SHA = "sha1" })
	js = h.tick()
	wantKinds(t, js, core.JobAutoReview)
	if js[0].HeadSHA != "sha1" || js[0].ForceFull {
		t.Errorf("push job %+v", js[0])
	}

	// Title only.
	h.touch(1, func(m *gitlab.MRSummary) { m.Title = "feat: y (PROJ-1)" })
	wantKinds(t, h.tick(), core.JobRefreshTitle)

	// Marked draft, pushed while draft: nothing.
	h.touch(1, func(m *gitlab.MRSummary) { m.Draft, m.Title = true, "Draft: feat: y (PROJ-1)" })
	wantKinds(t, h.tick())
	h.touch(1, func(m *gitlab.MRSummary) { m.SHA = "sha2" })
	wantKinds(t, h.tick())

	// Draft → Ready: full review of the current head.
	h.touch(1, func(m *gitlab.MRSummary) { m.Draft, m.Title = false, "feat: y (PROJ-1)" })
	js = h.tick()
	wantKinds(t, js, core.JobAutoReview)
	if !js[0].ForceFull || js[0].HeadSHA != "sha2" {
		t.Errorf("ready job %+v", js[0])
	}

	// Closed, then reopened.
	h.touch(1, func(m *gitlab.MRSummary) { m.State = "closed" })
	wantKinds(t, h.tick())
	h.touch(1, func(m *gitlab.MRSummary) { m.State = "opened" })
	wantKinds(t, h.tick(), core.JobAutoReview)

	// Merged MR gets nothing.
	h.touch(2, func(m *gitlab.MRSummary) { m.State = "merged"; m.SHA = "n2" })
	wantKinds(t, h.tick())
}

func TestNoteEvents(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.tick()

	h.note(1, "d1", true, 200, "alice", "@reviewbot review")
	h.touch(1, func(m *gitlab.MRSummary) {})
	js := h.tick()
	wantKinds(t, js, core.JobReview)
	if js[0].NoteID != 200 || js[0].Username != "alice" || js[0].UserID != 2000 || js[0].DiscussionID != "d1" {
		t.Errorf("command job %+v", js[0])
	}

	// Several notes at once, in ID order; the bot's own note is dropped; a
	// plain comment without a mention is dropped; a reply in a thread is a
	// thread reply (learning).
	h.note(1, "bot1", false, 300, "reviewbot", "<!-- reviewbot:finding id=x -->\nПроблема")
	h.note(1, "bot1", false, 302, "bob", "Это не баг: так задумано")
	h.note(1, "d2", true, 301, "carol", "просто комментарий")
	h.note(1, "d3", true, 303, "dave", "@reviewbot что делает эта функция?")
	h.touch(1, func(m *gitlab.MRSummary) {})
	js = h.tick()
	wantKinds(t, js, core.JobThreadReply, core.JobChat)
	if js[0].NoteID != 302 || js[0].DiscussionID != "bot1" || js[1].Question != "что делает эта функция?" {
		t.Errorf("jobs %+v", js)
	}

	// Nothing new.
	h.touch(1, func(m *gitlab.MRSummary) {})
	wantKinds(t, h.tick())

	// A push and a command in the same tick: the review first.
	h.note(1, "d4", true, 400, "alice", "@reviewbot пауза")
	h.touch(1, func(m *gitlab.MRSummary) { m.SHA = "sha9" })
	wantKinds(t, h.tick(), core.JobAutoReview, core.JobPause)
}

func TestFailedMRIsRetried(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.tick()
	before := h.st.cursors[""]

	h.note(1, "d1", true, 500, "alice", "@reviewbot review")
	h.touch(1, func(m *gitlab.MRSummary) {})
	h.gl.discErr[[2]int64{7, 1}] = errors.New("502")
	h.now = h.now.Add(30 * time.Second)
	h.jobs = nil
	if _, err := h.p.Tick(context.Background()); err != nil {
		t.Fatalf("a failed MR must not fail the tick: %v", err)
	}
	wantKinds(t, h.jobs)
	if c := h.st.cursors[""]; c.After(h.gl.mrs[[2]int64{7, 1}].UpdatedAt) || c.Before(before) {
		t.Fatalf("cursor moved past the failed MR: %v", c)
	}

	delete(h.gl.discErr, [2]int64{7, 1})
	wantKinds(t, h.tick(), core.JobReview)
	wantKinds(t, h.tick())
}

func TestGroupsAndCursorPerScope(t *testing.T) {
	h := newHarness(t, "team", "other")
	h.tick()
	if len(h.st.cursors) != 2 {
		t.Fatalf("cursors %+v", h.st.cursors)
	}
	// The fake returns the MR for both scopes; the second sees the same
	// updated_at and skips it, so one job.
	h.touch(3, func(m *gitlab.MRSummary) {})
	wantKinds(t, h.tick(), core.JobAutoReview)
	// The router keeps the group filter.
	h.touch(4, func(m *gitlab.MRSummary) { m.ProjectPath = "elsewhere/x" })
	wantKinds(t, h.tick())
	// Queries go from the cursor minus the overlap.
	last := h.gl.listCall[len(h.gl.listCall)-1]
	if got := h.st.cursors["other"].Sub(last); got < Overlap {
		t.Errorf("query since %v, cursor %v", last, h.st.cursors["other"])
	}
}

func TestDedup(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d := NewDedup(time.Hour, func() time.Time { return now })
	var got []core.Job
	enq := d.Wrap(func(j core.Job) { got = append(got, j) })

	push := core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 2, HeadSHA: "a"}
	enq(push)
	enq(push) // the same push from the other source
	ready := push
	ready.ForceFull = true
	enq(ready) // Draft → Ready of the same head is a different event
	enq(core.Job{Kind: core.JobChat, NoteID: 9})
	enq(core.Job{Kind: core.JobChat, NoteID: 9})
	enq(core.Job{Kind: core.JobRefreshTitle, ProjectID: 1, MRIID: 2})
	enq(core.Job{Kind: core.JobRefreshTitle, ProjectID: 1, MRIID: 2})
	if len(got) != 5 {
		t.Fatalf("got %d jobs: %v", len(got), kinds(got))
	}
	now = now.Add(2 * time.Hour)
	enq(push)
	if len(got) != 6 {
		t.Fatal("expired key must pass again")
	}
}

func TestExcludedProjectSkipped(t *testing.T) {
	h := newHarness(t)
	h.p.o.Router = events.NewRouter(events.Options{BotUsername: "reviewbot", Excluded: func(p string) bool { return p == "team/api" }})
	h.tick()
	h.gl.discErr[[2]int64{7, 1}] = errors.New("must not be called")
	h.touch(1, func(m *gitlab.MRSummary) {})
	h.note(1, "d", true, 9, "alice", "@reviewbot review")
	wantKinds(t, h.tick())
}

func TestDedupKeyPipeline(t *testing.T) {
	j := core.Job{Kind: core.JobPipeline, ProjectID: 1, MRIID: 7, Pipeline: &core.PipelineRef{ID: 9, Status: "failed"}}
	if Key(j) != "pipeline:1:9:7:failed" {
		t.Fatal(Key(j))
	}
	d := NewDedup(time.Hour, nil)
	if !d.First(j) || d.First(j) {
		t.Fatal("same pipeline and status twice")
	}
	j.Pipeline = &core.PipelineRef{ID: 9, Status: "success"} // retried job: green now
	if !d.First(j) {
		t.Fatal("a new status is a new event")
	}
	if Key(core.Job{Kind: core.JobPipeline}) != "" {
		t.Fatal("no pipeline: no key")
	}
}

// A pipeline that fails again after a job retry (failed -> success ->
// failed, or failed -> failed) is a new event; the webhook and the poller
// reporting one failure (finished_at vs updated_at, seconds apart) are not.
func TestDedupPipelineFailsAgain(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	d := NewDedup(24*time.Hour, func() time.Time { return now })
	job := func(status string, at time.Time) core.Job {
		return core.Job{Kind: core.JobPipeline, ProjectID: 1, MRIID: 7, Pipeline: &core.PipelineRef{ID: 9, Status: status, At: at}}
	}
	t0 := time.Date(2026, 10, 24, 10, 5, 0, 0, time.UTC)
	if !d.First(job("failed", t0)) {
		t.Fatal("first failure")
	}
	if d.First(job("failed", t0.Add(700*time.Millisecond))) { // the poller's updated_at of the same failure
		t.Fatal("the same failure from the other source passed")
	}
	if d.First(job("failed", t0)) {
		t.Fatal("the same failure twice passed")
	}
	if !d.First(job("success", t0.Add(3*time.Minute))) {
		t.Fatal("green after a retry")
	}
	if !d.First(job("failed", t0.Add(6*time.Minute))) {
		t.Fatal("failed again after a retry was dropped")
	}
	if !d.First(job("failed", t0.Add(9*time.Minute))) {
		t.Fatal("failed a third time (retry of a failed job) was dropped")
	}
	// Unknown times behave as before: one event per status.
	if !d.First(job("canceled", time.Time{})) || d.First(job("canceled", time.Time{})) {
		t.Fatal("unknown time")
	}
}

// GitLab 19.4 bumps updated_at for a comment at most once a minute: a
// command written soon after another update (a push, the bot's own note)
// leaves updated_at as it was. Such a comment is still found while the MR is
// hot, and an MR that stayed quiet longer is not read again.
func TestCommentWithoutUpdatedAtBump(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.tick()
	h.touch(1, func(m *gitlab.MRSummary) { m.SHA = "sha1" })
	wantKinds(t, h.tick(), core.JobAutoReview)

	h.note(1, "d1", true, 200, "alice", "@reviewbot review") // updated_at not bumped
	js := h.tick()
	wantKinds(t, js, core.JobReview)
	if js[0].NoteID != 200 {
		t.Errorf("job %+v", js[0])
	}
	wantKinds(t, h.tick()) // found once, not again

	for i := 0; i < 6; i++ { // quiet for longer than HotWindow
		h.tick()
	}
	h.gl.discRead = 0
	h.note(1, "d2", true, 201, "alice", "@reviewbot review")
	wantKinds(t, h.tick())
	if h.gl.discRead != 0 {
		t.Errorf("a quiet MR was read again: %d discussion calls", h.gl.discRead)
	}
}

// A comment that did not bump updated_at (see above) written while the bot
// was down is found by the first tick after the restart, however long the
// downtime: the MR is no longer hot, but a fresh process reads the
// discussions of the MRs of the last CatchUp.
func TestRestartCatchesCommentWithoutBump(t *testing.T) {
	h := newHarness(t)
	h.mr(1)
	h.tick()
	h.touch(1, func(m *gitlab.MRSummary) { m.SHA = "sha1" })
	wantKinds(t, h.tick(), core.JobAutoReview)
	for i := 0; i < 8; i++ { // the MR goes cold: more than HotWindow
		h.tick()
	}

	h.note(1, "d1", true, 200, "alice", "@reviewbot review") // written while "down": no bump
	h.now = h.now.Add(10 * time.Minute)
	h.p = New(Options{
		Source: h.gl, State: h.st,
		Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", LearnReplies: true}),
		Enqueue: func(j core.Job) { h.jobs = append(h.jobs, j) },
		Now:     func() time.Time { return h.now },
	})
	js := h.tick()
	wantKinds(t, js, core.JobReview)
	if js[0].NoteID != 200 {
		t.Errorf("job %+v", js[0])
	}
	h.gl.discRead = 0
	wantKinds(t, h.tick())
	if h.gl.discRead != 0 {
		t.Errorf("the catch-up repeated on the second tick: %d discussion calls", h.gl.discRead)
	}
}
