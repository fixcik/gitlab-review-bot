package poller

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
)

func TestNewPipelinesDefaults(t *testing.T) {
	p := NewPipelines(PipelineOptions{})
	if p.now == nil || p.log == nil || p.o.Interval != time.Minute || p.o.ProjectsEvery != 10*time.Minute {
		t.Fatalf("defaults: interval %v, projects every %v", p.o.Interval, p.o.ProjectsEvery)
	}
	if d := time.Since(p.now()); d < 0 || d > time.Minute {
		t.Fatalf("default clock is not time.Now: %v", d)
	}
	p = NewPipelines(PipelineOptions{Interval: time.Second, ProjectsEvery: time.Hour})
	if p.o.Interval != time.Second || p.o.ProjectsEvery != time.Hour {
		t.Fatalf("options overridden: %v %v", p.o.Interval, p.o.ProjectsEvery)
	}
}

// twoProjects: a pipeline poller over team/a (1) and team/b (2), both with
// cursors already set at cursor.
func twoProjects(src *fakePipelines, cursor time.Time) (*PipelinePoller, *faultState, *[]core.Job) {
	if src.projects == nil {
		src.projects = map[string][]gitlab.ProjectDetails{"team": {
			{ID: 1, PathWithNamespace: "team/a", DefaultBranch: "main"},
			{ID: 2, PathWithNamespace: "team/b", DefaultBranch: "main"},
		}}
	}
	if src.pipelines == nil {
		src.pipelines = map[int64][]gitlab.PipelineInfo{}
	}
	if src.projectErr == nil {
		src.projectErr = map[int64]error{}
	}
	st := &faultState{memState: newMem()}
	st.cursors[pipelineScope(1)] = cursor
	st.cursors[pipelineScope(2)] = cursor
	var jobs []core.Job
	now := cursor.Add(time.Minute)
	p := NewPipelines(PipelineOptions{
		Source: src, State: st, Groups: []string{"team"},
		Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", Groups: []string{"team"}, Pipelines: true}),
		Enqueue: func(j core.Job) { jobs = append(jobs, j) },
		Now:     func() time.Time { return now },
	})
	return p, st, &jobs
}

var pipeT0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// Run ticks at once and then every interval until the context ends.
func TestPipelinePollerRunTicks(t *testing.T) {
	p, st, _ := twoProjects(&fakePipelines{}, pipeT0)
	p.o.Interval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for st.cursorRead.Load() < 6 { // two projects, three ticks
		select {
		case <-deadline:
			t.Fatalf("only %d cursor reads", st.cursorRead.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with the context")
	}
}

// A project failing because the context ended stops the tick: the next
// projects are not polled, the context's error is returned.
func TestPipelineTickStopsOnCancelledContext(t *testing.T) {
	src := &fakePipelines{}
	p, st, _ := twoProjects(src, pipeT0)
	ctx, cancel := context.WithCancel(context.Background())
	st.getCursor = func(string) error { cancel(); return errors.New("interrupted") }
	_, err := p.Tick(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if n := st.cursorRead.Load(); n != 1 {
		t.Fatalf("%d projects polled after the cancel", n)
	}
}

// A store error reading a project's cursor is an error (not a reason to
// re-baseline): no listing, the cursor is kept, other projects are polled.
func TestPipelineCursorReadError(t *testing.T) {
	src := &fakePipelines{}
	src.pipelines = map[int64][]gitlab.PipelineInfo{
		1: {{ID: 10, Ref: "refs/merge-requests/3/head", SHA: "a", Status: "failed", UpdatedAt: pipeT0.Add(10 * time.Second)}},
		2: {{ID: 20, Ref: "refs/merge-requests/4/head", SHA: "b", Status: "failed", UpdatedAt: pipeT0.Add(10 * time.Second)}},
	}
	p, st, jobs := twoProjects(src, pipeT0)
	st.getCursor = failOn(pipelineScope(1), errors.New("db locked"))
	n, err := p.Tick(context.Background())
	if err == nil || err.Error() != "db locked" || n != 1 {
		t.Fatalf("tick: %d %v", n, err)
	}
	if len(*jobs) != 1 || (*jobs)[0].Pipeline.ID != 20 {
		t.Fatalf("jobs %+v", *jobs)
	}
	if !st.cursors[pipelineScope(1)].Equal(pipeT0) {
		t.Fatalf("cursor of the failed project rewritten: %v", st.cursors[pipelineScope(1)])
	}
	for _, q := range src.queries {
		if q.UpdatedAfter.IsZero() {
			t.Fatal("re-baselined from the beginning of time")
		}
	}
	if len(src.queries) != 1 {
		t.Fatalf("%d listings, want only project 2", len(src.queries))
	}
}

// A store error saving the moved cursor is the tick's error; the jobs read
// were enqueued and are counted, the cursor stays (they are re-read and the
// dedup drops them).
func TestPipelineCursorSaveError(t *testing.T) {
	src := &fakePipelines{}
	src.pipelines = map[int64][]gitlab.PipelineInfo{
		1: {{ID: 10, Ref: "refs/merge-requests/3/head", SHA: "a", Status: "failed", UpdatedAt: pipeT0.Add(10 * time.Second)}},
	}
	p, st, jobs := twoProjects(src, pipeT0)
	st.setCursor = failOn(pipelineScope(1), errors.New("disk full"))
	n, err := p.Tick(context.Background())
	if err == nil || err.Error() != "disk full" || n != 1 || len(*jobs) != 1 {
		t.Fatalf("tick: %d %v %+v", n, err, *jobs)
	}
	if !st.cursors[pipelineScope(1)].Equal(pipeT0) {
		t.Fatalf("cursor moved: %v", st.cursors[pipelineScope(1)])
	}
}

// staleSource returns full pages of pipelines updated long before the
// requested window (a GitLab ignoring updated_after).
type staleSource struct {
	fakePipelines
	at time.Time
}

func (s *staleSource) ListPipelines(_ context.Context, _ int64, q gitlab.PipelineQuery) ([]gitlab.PipelineInfo, error) {
	out := make([]gitlab.PipelineInfo, q.Limit)
	for i := range out {
		out[i] = gitlab.PipelineInfo{ID: int64(q.Page*1000 + i), Ref: "main", Status: "running", UpdatedAt: s.at}
	}
	return out, nil
}

// When a tick hits the page cap and the last pipeline read is not even past
// the cursor, the cursor still moves (by a second): the poller never stalls
// on the same window.
func TestPipelineCapMovesCursorEvenOnStaleListing(t *testing.T) {
	src := &staleSource{at: pipeT0.Add(-time.Hour)}
	src.projects = map[string][]gitlab.ProjectDetails{"team": {
		{ID: 1, PathWithNamespace: "team/a", DefaultBranch: "main"},
		{ID: 2, PathWithNamespace: "team/b", DefaultBranch: "main"},
	}}
	st := &faultState{memState: newMem()}
	st.cursors[pipelineScope(1)] = pipeT0
	st.cursors[pipelineScope(2)] = pipeT0
	p := NewPipelines(PipelineOptions{Source: src, State: st, Groups: []string{"team"},
		Router: events.NewRouter(events.Options{BotUsername: "reviewbot", Pipelines: true}),
		Now:    func() time.Time { return pipeT0.Add(time.Minute) }})
	if _, err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if c := st.cursors[pipelineScope(id)]; !c.Equal(pipeT0.Add(time.Second)) {
			t.Fatalf("project %d: cursor %v, want %v", id, c, pipeT0.Add(time.Second))
		}
	}
}

// Any positive interval is kept, however small.
func TestNewPipelinesKeepsTinyIntervals(t *testing.T) {
	p := NewPipelines(PipelineOptions{Interval: 1, ProjectsEvery: 1})
	if p.o.Interval != 1 || p.o.ProjectsEvery != 1 {
		t.Fatalf("%v %v", p.o.Interval, p.o.ProjectsEvery)
	}
}

func failedMRPipeline(id, mr int64, at time.Time) gitlab.PipelineInfo {
	return gitlab.PipelineInfo{ID: id, Ref: "refs/merge-requests/" + strconv.FormatInt(mr, 10) + "/head", SHA: "a",
		Status: "failed", UpdatedAt: at}
}

// An excluded project anywhere in the list is skipped without stopping the
// others; listing errors count no jobs and the tick reports the first one.
func TestPipelineTickSkipsAndFirstError(t *testing.T) {
	src := &fakePipelines{projects: map[string][]gitlab.ProjectDetails{"team": {
		{ID: 5, PathWithNamespace: "team/skip", DefaultBranch: "main"},
		{ID: 1, PathWithNamespace: "team/a", DefaultBranch: "main"},
		{ID: 2, PathWithNamespace: "team/b", DefaultBranch: "main"},
		{ID: 3, PathWithNamespace: "team/c", DefaultBranch: "main"},
	}}}
	src.pipelines = map[int64][]gitlab.PipelineInfo{3: {failedMRPipeline(30, 3, pipeT0.Add(time.Second))}}
	src.projectErr = map[int64]error{1: errors.New("a: 500"), 2: errors.New("b: 502")}
	p, st, jobs := twoProjects(src, pipeT0)
	st.cursors[pipelineScope(3)] = pipeT0
	st.cursors[pipelineScope(5)] = pipeT0
	p.o.Router = events.NewRouter(events.Options{BotUsername: "reviewbot", Pipelines: true,
		Excluded: func(path string) bool { return path == "team/skip" }})
	n, err := p.Tick(context.Background())
	if err == nil || err.Error() != "a: 500" {
		t.Fatalf("err %v, want the first project's", err)
	}
	if n != 1 || len(*jobs) != 1 || (*jobs)[0].Pipeline.ID != 30 {
		t.Fatalf("n %d, jobs %+v", n, *jobs)
	}
	if st.cursorRead.Load() != 3 {
		t.Fatalf("%d cursor reads, want the 3 accepted projects", st.cursorRead.Load())
	}
}

// The project list is re-read once ProjectsEvery has passed (not before).
func TestPipelineProjectsRefreshInterval(t *testing.T) {
	src := &fakePipelines{}
	p, _, _ := twoProjects(src, pipeT0)
	now := pipeT0
	p.now = func() time.Time { return now }
	p.o.ProjectsEvery = 10 * time.Minute
	p.Tick(context.Background())
	if src.listCalls != 1 {
		t.Fatalf("first tick: %d listings", src.listCalls)
	}
	now = now.Add(10*time.Minute - time.Nanosecond)
	p.Tick(context.Background())
	if src.listCalls != 1 {
		t.Fatalf("re-read before ProjectsEvery: %d", src.listCalls)
	}
	now = pipeT0.Add(10 * time.Minute)
	p.Tick(context.Background())
	if src.listCalls != 2 {
		t.Fatalf("not re-read at ProjectsEvery: %d", src.listCalls)
	}
}

// groupFailSource fails the project listing of one group.
type groupFailSource struct {
	*fakePipelines
	failGroup string
}

func (s *groupFailSource) ListGroupProjectsDetails(ctx context.Context, g string) ([]gitlab.ProjectDetails, error) {
	if g == s.failGroup {
		return nil, errors.New("403")
	}
	return s.fakePipelines.ListGroupProjectsDetails(ctx, g)
}

// A group whose project listing fails keeps its last list; the next groups
// are still refreshed.
func TestPipelineFailedRefreshKeepsLastList(t *testing.T) {
	fp := &fakePipelines{projects: map[string][]gitlab.ProjectDetails{
		"one": {{ID: 1, PathWithNamespace: "one/a", DefaultBranch: "main"}},
		"two": {{ID: 2, PathWithNamespace: "two/b", DefaultBranch: "main"}},
	}, pipelines: map[int64][]gitlab.PipelineInfo{}, projectErr: map[int64]error{}}
	src := &groupFailSource{fakePipelines: fp}
	st := &faultState{memState: newMem()}
	now := pipeT0
	p := NewPipelines(PipelineOptions{Source: src, State: st, Groups: []string{"one", "two"}, ProjectsEvery: time.Minute,
		Router: events.NewRouter(events.Options{BotUsername: "reviewbot", Pipelines: true}),
		Now:    func() time.Time { return now }})
	p.Tick(context.Background()) // baseline of 1 and 2
	fp.projects["one"] = append(fp.projects["one"], gitlab.ProjectDetails{ID: 3, PathWithNamespace: "one/c", DefaultBranch: "main"})
	fp.projects["two"] = append(fp.projects["two"], gitlab.ProjectDetails{ID: 4, PathWithNamespace: "two/d", DefaultBranch: "main"})
	src.failGroup = "one"
	now = now.Add(time.Minute)
	reads := st.cursorRead.Load()
	p.Tick(context.Background())
	if n := st.cursorRead.Load() - reads; n != 3 {
		t.Errorf("%d projects polled, want 1 (kept), 2 and 4 (refreshed)", n)
	}
	for _, id := range []int64{1, 2, 4} {
		if _, ok := st.cursors[pipelineScope(id)]; !ok {
			t.Errorf("project %d not polled", id)
		}
	}
	if _, ok := st.cursors[pipelineScope(3)]; ok {
		t.Error("project of the failed group listing appeared")
	}
}

// Without an Enqueue the accepted pipelines are still counted (dry run).
func TestPipelineNilEnqueueCounts(t *testing.T) {
	src := &fakePipelines{}
	src.pipelines = map[int64][]gitlab.PipelineInfo{1: {failedMRPipeline(10, 3, pipeT0.Add(time.Second))}}
	p, st, _ := twoProjects(src, pipeT0)
	p.o.Enqueue = nil
	if n, err := p.Tick(context.Background()); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if !st.cursors[pipelineScope(1)].Equal(pipeT0.Add(time.Second)) {
		t.Fatalf("cursor %v", st.cursors[pipelineScope(1)])
	}
}

// A tick with nothing new does not write the cursor.
func TestPipelineNoNewsNoCursorWrite(t *testing.T) {
	src := &fakePipelines{}
	src.pipelines = map[int64][]gitlab.PipelineInfo{1: {failedMRPipeline(10, 3, pipeT0.Add(-time.Minute))}} // inside the overlap, older than the cursor
	p, st, jobs := twoProjects(src, pipeT0)
	st.setCursor = func(string) error { return errors.New("read-only store") }
	if n, err := p.Tick(context.Background()); err != nil || n != 1 || len(*jobs) != 1 {
		t.Fatalf("%d %v", n, err)
	}
}

// After a capped tick the cursor sits exactly Overlap after the last
// pipeline read, so the next query starts right after it.
func TestPipelineCapCursorExact(t *testing.T) {
	now := pipeT0
	src := &pagedPipelines{}
	p, st, _ := pagedPoller(src, &now)
	p.Tick(context.Background()) // baseline
	base := now
	for i := 0; i < pipelinesPerCall*pipelinePagesPerTick+10; i++ {
		src.ps = append(src.ps, gitlab.PipelineInfo{ID: int64(1 + i), Ref: "feature", Status: "canceled",
			UpdatedAt: base.Add(time.Duration(i) * time.Second)})
	}
	now = now.Add(time.Minute)
	p.Tick(context.Background())
	want := base.Add(time.Duration(pipelinesPerCall*pipelinePagesPerTick-1)*time.Second + Overlap)
	if c := st.cursors["pipelines:1"]; !c.Equal(want) {
		t.Fatalf("cursor %v, want %v", c, want)
	}
}

// A project in two configured groups (a subgroup listed too) is polled once
// per tick, under the scope "pipelines:<id>"; the projects after it are
// still polled.
func TestPipelineProjectInTwoGroupsPolledOnce(t *testing.T) {
	pr := gitlab.ProjectDetails{ID: 42, PathWithNamespace: "team/sub/api", DefaultBranch: "main"}
	other := gitlab.ProjectDetails{ID: 43, PathWithNamespace: "team/sub/web", DefaultBranch: "main"}
	src := &fakePipelines{projects: map[string][]gitlab.ProjectDetails{"team": {pr}, "team/sub": {pr, other}},
		pipelines: map[int64][]gitlab.PipelineInfo{42: {failedMRPipeline(10, 3, pipeT0.Add(time.Second))}}}
	st := &faultState{memState: newMem()}
	st.cursors["pipelines:42"] = pipeT0
	st.cursors["pipelines:43"] = pipeT0
	var jobs []core.Job
	p := NewPipelines(PipelineOptions{Source: src, State: st, Groups: []string{"team", "team/sub"},
		Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", Pipelines: true}),
		Enqueue: func(j core.Job) { jobs = append(jobs, j) },
		Now:     func() time.Time { return pipeT0.Add(time.Minute) }})
	if n, err := p.Tick(context.Background()); n != 1 || err != nil || len(jobs) != 1 || st.cursorRead.Load() != 2 {
		t.Fatalf("%d %v, %d jobs, %d cursor reads", n, err, len(jobs), st.cursorRead.Load())
	}
	if c, ok := st.cursors["pipelines:42"]; !ok || !c.Equal(pipeT0.Add(time.Second)) || len(st.cursors) != 2 {
		t.Fatalf("cursors %v", st.cursors)
	}
}
