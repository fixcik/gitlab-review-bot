package poller

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
)

type fakePipelines struct {
	projects   map[string][]gitlab.ProjectDetails
	listErr    error
	pipelines  map[int64][]gitlab.PipelineInfo
	listCalls  int
	queries    []gitlab.PipelineQuery
	projectErr map[int64]error
}

func (f *fakePipelines) ListGroupProjectsDetails(_ context.Context, g string) ([]gitlab.ProjectDetails, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.projects[g], nil
}

func (f *fakePipelines) ListPipelines(_ context.Context, pid int64, q gitlab.PipelineQuery) ([]gitlab.PipelineInfo, error) {
	f.queries = append(f.queries, q)
	if err := f.projectErr[pid]; err != nil {
		return nil, err
	}
	var out []gitlab.PipelineInfo
	for _, p := range f.pipelines[pid] {
		if !p.UpdatedAt.Before(q.UpdatedAfter) {
			out = append(out, p)
		}
	}
	return out, nil
}

func TestPipelinePoller(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	src := &fakePipelines{
		projects: map[string][]gitlab.ProjectDetails{"team": {
			{ID: 1, PathWithNamespace: "team/api", DefaultBranch: "main"},
			{ID: 2, PathWithNamespace: "team/web", DefaultBranch: "master"},
			{ID: 3, PathWithNamespace: "team/empty"}, // empty repository: skipped
			{ID: 4, PathWithNamespace: "team/skip", DefaultBranch: "main"},
		}},
		pipelines:  map[int64][]gitlab.PipelineInfo{},
		projectErr: map[int64]error{},
	}
	st := newMem()
	var jobs []core.Job
	p := NewPipelines(PipelineOptions{
		Source: src, State: st, Groups: []string{"team"},
		Router: events.NewRouter(events.Options{BotUsername: "reviewbot", Groups: []string{"team"}, Pipelines: true,
			Excluded: func(p string) bool { return p == "team/skip" }}),
		Enqueue: func(j core.Job) { jobs = append(jobs, j) },
		Now:     func() time.Time { return now },
	})
	// First tick: baseline only.
	if n, err := p.Tick(context.Background()); n != 0 || err != nil {
		t.Fatalf("baseline: %d %v", n, err)
	}
	if _, ok := st.cursors["pipelines:1"]; !ok {
		t.Fatal("no cursor after the baseline")
	}
	if _, ok := st.cursors["pipelines:4"]; ok {
		t.Fatal("excluded project polled")
	}
	if _, ok := st.cursors["pipelines:3"]; ok {
		t.Fatal("empty project polled")
	}
	now = now.Add(time.Minute)
	src.pipelines[1] = []gitlab.PipelineInfo{
		{ID: 10, Ref: "main", SHA: "aaa", Status: "failed", UpdatedAt: now.Add(-30 * time.Second)},
		{ID: 11, Ref: "feature", SHA: "bbb", Status: "running", UpdatedAt: now.Add(-20 * time.Second)},
		{ID: 12, Ref: "refs/merge-requests/7/head", SHA: "ccc", Status: "success", UpdatedAt: now.Add(-10 * time.Second)},
	}
	src.pipelines[2] = []gitlab.PipelineInfo{{ID: 20, Ref: "master", SHA: "ddd", Status: "success", UpdatedAt: now.Add(-5 * time.Second)}}
	n, err := p.Tick(context.Background())
	if err != nil || n != 3 || len(jobs) != 3 {
		t.Fatalf("tick: %d %v %+v", n, err, jobs)
	}
	if j := jobs[0]; j.Kind != core.JobPipeline || !j.Pipeline.Main || j.Pipeline.ID != 10 || j.MRIID != 0 {
		t.Errorf("main job %+v %+v", j, j.Pipeline)
	}
	if j := jobs[1]; j.MRIID != 7 || j.Pipeline.Main || j.Pipeline.Status != "success" {
		t.Errorf("mr job %+v %+v", j, j.Pipeline)
	}
	if j := jobs[2]; !j.Pipeline.Main || j.Pipeline.Ref != "master" {
		t.Errorf("default branch master: %+v", j.Pipeline)
	}
	if c := st.cursors["pipelines:1"]; !c.Equal(now.Add(-10 * time.Second)) {
		t.Errorf("cursor %v", c)
	}
	q := src.queries[len(src.queries)-1]
	if q.OrderBy != "updated_at" || q.Sort != "asc" || q.Limit != 100 {
		t.Errorf("query %+v", q)
	}
	// The project list is cached; a failing project does not stop the others.
	calls := src.listCalls
	src.projectErr[1] = errors.New("boom")
	if _, err := p.Tick(context.Background()); err == nil {
		t.Error("error not reported")
	}
	if src.listCalls != calls {
		t.Error("project list re-read before ProjectsEvery")
	}
	now = now.Add(11 * time.Minute)
	src.listErr = errors.New("down")
	delete(src.projectErr, 1)
	if _, err := p.Tick(context.Background()); err != nil {
		t.Fatalf("a failed project list keeps the last one: %v", err)
	}
	if src.listCalls == calls {
		t.Error("project list not refreshed")
	}
}

// pagedPipelines behaves like GitLab: updated_after (strictly after),
// order_by updated_at asc, pages of Limit.
type pagedPipelines struct{ ps []gitlab.PipelineInfo }

func (f *pagedPipelines) ListGroupProjectsDetails(context.Context, string) ([]gitlab.ProjectDetails, error) {
	return []gitlab.ProjectDetails{{ID: 1, PathWithNamespace: "team/api", DefaultBranch: "main"}}, nil
}

func (f *pagedPipelines) ListPipelines(_ context.Context, _ int64, q gitlab.PipelineQuery) ([]gitlab.PipelineInfo, error) {
	var out []gitlab.PipelineInfo
	for _, p := range f.ps {
		if p.UpdatedAt.After(q.UpdatedAfter) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	page := q.Page
	if page < 1 {
		page = 1
	}
	from := (page - 1) * q.Limit
	if from >= len(out) {
		return nil, nil
	}
	out = out[from:]
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func pagedPoller(src *pagedPipelines, now *time.Time) (*PipelinePoller, *memState, *[]core.Job) {
	st := newMem()
	var jobs []core.Job
	p := NewPipelines(PipelineOptions{
		Source: src, State: st, Groups: []string{"team"},
		Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", Groups: []string{"team"}, Pipelines: true}),
		Enqueue: func(j core.Job) { jobs = append(jobs, j) },
		Now:     func() time.Time { return *now },
	})
	return p, st, &jobs
}

func enqueued(jobs []core.Job, id int64) bool {
	for _, j := range jobs {
		if j.Pipeline != nil && j.Pipeline.ID == id {
			return true
		}
	}
	return false
}

// More pipelines updated within the overlap window than one page holds
// must not starve the later ones (the cursor used to stay put forever).
func TestPipelinePollerPagesPastABurst(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	src := &pagedPipelines{}
	p, st, jobs := pagedPoller(src, &now)
	p.Tick(context.Background()) // baseline
	base := now
	for i := 0; i < 120; i++ {
		src.ps = append(src.ps, gitlab.PipelineInfo{ID: int64(100 + i), Ref: "feature", SHA: "a", Status: "canceled",
			UpdatedAt: base.Add(time.Duration(i) * 500 * time.Millisecond)})
	}
	for tick := 0; tick < 3; tick++ {
		now = now.Add(time.Minute)
		p.Tick(context.Background())
	}
	src.ps = append(src.ps, gitlab.PipelineInfo{ID: 999, Ref: "refs/merge-requests/7/head", SHA: "b", Status: "failed",
		UpdatedAt: base.Add(10 * time.Minute)})
	for tick := 0; tick < 3; tick++ {
		now = now.Add(time.Minute)
		p.Tick(context.Background())
	}
	if !enqueued(*jobs, 999) {
		t.Fatalf("failed pipeline 999 never enqueued: cursor %v", st.cursors["pipelines:1"])
	}
	if j := (*jobs)[len(*jobs)-1]; !j.Pipeline.At.Equal(base.Add(10 * time.Minute)) {
		t.Errorf("the job lacks the pipeline's updated_at: %+v", j.Pipeline)
	}
}

// A burst larger than one tick reads: the cursor moves past the last
// pipeline read (no overlap), every pipeline is seen within a few ticks and
// a later failure is not starved.
func TestPipelinePollerCapNeverStalls(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	src := &pagedPipelines{}
	p, st, jobs := pagedPoller(src, &now)
	p.Tick(context.Background()) // baseline
	base := now
	n := pipelinesPerCall*pipelinePagesPerTick + 500
	for i := 0; i < n; i++ {
		src.ps = append(src.ps, gitlab.PipelineInfo{ID: int64(10_000 + i), Ref: "refs/merge-requests/3/head", SHA: "a", Status: "failed",
			UpdatedAt: base.Add(time.Duration(i) * time.Millisecond)})
	}
	now = now.Add(time.Minute)
	p.Tick(context.Background())
	if c := st.cursors["pipelines:1"]; !c.After(base) {
		t.Fatalf("cursor did not move past the burst: %v", c)
	}
	src.ps = append(src.ps, gitlab.PipelineInfo{ID: 999, Ref: "refs/merge-requests/7/head", SHA: "b", Status: "failed",
		UpdatedAt: base.Add(5 * time.Minute)})
	for tick := 0; tick < 6; tick++ {
		now = now.Add(time.Minute)
		p.Tick(context.Background())
	}
	seen := map[int64]bool{}
	for _, j := range *jobs {
		seen[j.Pipeline.ID] = true
	}
	for i := 0; i < n; i++ {
		if !seen[int64(10_000+i)] {
			t.Fatalf("pipeline %d of the burst never enqueued (%d seen)", 10_000+i, len(seen))
		}
	}
	if !seen[999] {
		t.Fatalf("later pipeline starved: cursor %v", st.cursors["pipelines:1"])
	}
}
