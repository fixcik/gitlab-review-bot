package contextrepos

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/repo"
)

type fakeLister struct {
	mu      sync.Mutex
	calls   int
	data    map[string][]gitlab.ProjectDetails
	fail    map[string]bool
	single  map[string]*gitlab.ProjectDetails // GetProjectDetails; absent = not found
	lookups int
}

func (f *fakeLister) GetProjectDetails(_ context.Context, path string) (*gitlab.ProjectDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if d, ok := f.single[path]; ok {
		return d, nil
	}
	return nil, core.ErrNotFound
}

func (f *fakeLister) ListGroupProjectsDetails(_ context.Context, g string) ([]gitlab.ProjectDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail[g] {
		return nil, errors.New("boom")
	}
	return f.data[g], nil
}

type fakeSnaps struct {
	mu    sync.Mutex
	specs []repo.SnapshotSpec
	opts  repo.SnapshotOptions
	fail  map[string]bool
	// failBranch: "path@branch" specs that fail (missing branch).
	failBranch map[string]bool
	calls      int
}

func (f *fakeSnaps) Snapshots(_ context.Context, specs []repo.SnapshotSpec, o repo.SnapshotOptions) []*repo.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs, f.opts = specs, o
	f.calls++
	var out []*repo.Snapshot
	for i := len(specs) - 1; i >= 0; i-- { // reversed: the provider must restore the order
		s := specs[i]
		if f.fail[s.Path] || f.failBranch[s.Path+"@"+s.DefaultBranch] {
			continue
		}
		out = append(out, &repo.Snapshot{Path: s.Path, Branch: s.DefaultBranch, SHA: "sha-" + s.Path, Dir: "/snap/" + s.Path})
	}
	return out
}

func paths(specs []repo.SnapshotSpec) string {
	var s []string
	for _, x := range specs {
		s = append(s, x.Path)
	}
	return strings.Join(s, ",")
}

func TestAcquireFiltersAndCaches(t *testing.T) {
	l := &fakeLister{data: map[string][]gitlab.ProjectDetails{
		"backend": {
			{PathWithNamespace: "backend/api", DefaultBranch: "main"},
			{PathWithNamespace: "backend/billing", DefaultBranch: "master", RepositorySize: 10},
			{PathWithNamespace: "backend/old", DefaultBranch: "main", Archived: true},
			{PathWithNamespace: "backend/empty"},
			{PathWithNamespace: "backend/huge", DefaultBranch: "main", RepositorySize: 2 << 20},
		},
		"platform": {
			{PathWithNamespace: "backend/billing", DefaultBranch: "master"}, // dup
			{PathWithNamespace: "platform/libs", DefaultBranch: "main"},
		},
	}}
	s := &fakeSnaps{}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := New(Options{Lister: l, Snapshots: s, Groups: []string{"backend", "platform"}, MaxRepoBytes: 1 << 20,
		RefreshInterval: 10 * time.Minute, Now: func() time.Time { return now }})

	dirs, _, release := p.Acquire(context.Background(), core.ContextRequest{Exclude: func(path string) bool { return path == "backend/api" }})
	defer release()
	if got := paths(s.specs); got != "backend/billing,platform/libs" {
		t.Fatalf("specs %s", got)
	}
	if s.specs[0].DefaultBranch != "master" || s.opts.RefreshInterval != 10*time.Minute || s.opts.MaxBytes != 1<<20 {
		t.Errorf("spec/opts %+v %+v", s.specs[0], s.opts)
	}
	if len(dirs) != 2 || dirs[0].Name != "backend/billing" || dirs[0].HostPath != "/snap/backend/billing" || dirs[0].SHA != "sha-backend/billing" || dirs[0].Branch != "master" {
		t.Errorf("dirs %+v", dirs)
	}
	if l.calls != 2 {
		t.Errorf("list calls %d", l.calls)
	}
	// Cached within the interval.
	_, _, r2 := p.Acquire(context.Background(), core.ContextRequest{})
	r2()
	if l.calls != 2 {
		t.Errorf("list must be cached: %d calls", l.calls)
	}
	// A failing group keeps its previous list after the interval.
	now = now.Add(11 * time.Minute)
	l.fail = map[string]bool{"platform": true}
	_, _, r3 := p.Acquire(context.Background(), core.ContextRequest{})
	r3()
	if l.calls != 4 || paths(s.specs) != "backend/api,backend/billing,platform/libs" {
		t.Errorf("calls %d specs %s", l.calls, paths(s.specs))
	}
}

func TestAcquireMaxRepos(t *testing.T) {
	l := &fakeLister{data: map[string][]gitlab.ProjectDetails{"g": {
		{PathWithNamespace: "g/c", DefaultBranch: "main"},
		{PathWithNamespace: "g/a", DefaultBranch: "main"},
		{PathWithNamespace: "g/b", DefaultBranch: "main"},
		{PathWithNamespace: "g/bad path", DefaultBranch: "main"},
	}}}
	s := &fakeSnaps{}
	p := New(Options{Lister: l, Snapshots: s, Groups: []string{"g"}, MaxRepos: 2})
	_, _, release := p.Acquire(context.Background(), core.ContextRequest{})
	release()
	if got := paths(s.specs); got != "g/a,g/b" {
		t.Errorf("specs %s", got)
	}
}

func TestAcquireNothing(t *testing.T) {
	l := &fakeLister{fail: map[string]bool{"g": true}}
	s := &fakeSnaps{}
	p := New(Options{Lister: l, Snapshots: s, Groups: []string{"g"}})
	dirs, _, release := p.Acquire(context.Background(), core.ContextRequest{})
	release()
	if len(dirs) != 0 || s.specs != nil {
		t.Errorf("dirs %v specs %v", dirs, s.specs)
	}
}

func TestAcquireRelated(t *testing.T) {
	l := &fakeLister{
		data: map[string][]gitlab.ProjectDetails{"g": {
			{PathWithNamespace: "g/a", DefaultBranch: "main"},
			{PathWithNamespace: "g/b", DefaultBranch: "main"},
			{PathWithNamespace: "g/web", DefaultBranch: "main"},
			{PathWithNamespace: "g/c", DefaultBranch: "main"},
		}},
		single: map[string]*gitlab.ProjectDetails{
			"front/app":   {PathWithNamespace: "front/app", DefaultBranch: "develop"},
			"front/empty": {PathWithNamespace: "front/empty"},
			"front/huge":  {PathWithNamespace: "front/huge", DefaultBranch: "main", RepositorySize: 5 << 20},
			"front/flaky": {PathWithNamespace: "front/flaky", DefaultBranch: "main"},
		},
	}
	s := &fakeSnaps{fail: map[string]bool{"front/flaky": true}}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := New(Options{Lister: l, Snapshots: s, Groups: []string{"g"}, MaxRepos: 3, MaxRepoBytes: 1 << 20,
		RefreshInterval: 10 * time.Minute, Now: func() time.Time { return now }})

	req := core.ContextRequest{
		// The repo excludes g/web, but names it as related: related wins.
		Exclude: func(path string) bool { return path == "g/a" || path == "g/web" },
		Related: []string{"G/Web", "front/app", "secret/repo", "front/empty", "front/huge", "front/flaky", "g/web"},
	}
	dirs, missing, release := p.Acquire(context.Background(), req)
	defer release()
	// Related first (in config order), then the groups up to MaxRepos in
	// total (3 related specs already reach it).
	if got := paths(s.specs); got != "g/web,front/app,front/flaky" {
		t.Fatalf("specs %s", got)
	}
	var names []string
	for _, d := range dirs {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "g/web,front/app" || dirs[1].Branch != "develop" {
		t.Fatalf("dirs %+v", dirs)
	}
	want := map[string]string{"secret/repo": ReasonNotFound, "front/empty": ReasonEmpty, "front/huge": ReasonTooLarge, "front/flaky": ReasonSnapshot}
	if len(missing) != len(want) {
		t.Fatalf("missing %+v", missing)
	}
	for _, m := range missing {
		if want[m.Path] != m.Reason {
			t.Errorf("missing %s: %q, want %q", m.Path, m.Reason, want[m.Path])
		}
	}

	// Lookups are cached: found ones for RefreshInterval, failures for a minute.
	n := l.lookups
	_, _, r := p.Acquire(context.Background(), core.ContextRequest{Related: []string{"front/app", "secret/repo"}})
	r()
	if l.lookups != n {
		t.Errorf("lookups not cached: %d -> %d", n, l.lookups)
	}
	now = now.Add(2 * time.Minute)
	_, _, r = p.Acquire(context.Background(), core.ContextRequest{Related: []string{"front/app", "secret/repo"}})
	r()
	if l.lookups != n+1 {
		t.Errorf("failed lookup must be retried after a minute: %d -> %d", n, l.lookups)
	}

	// Projects outside the groups stay in the warm set for a day after the
	// last real run; the warm pass itself does not extend that.
	if got := strings.Join(p.recentExtra(), ","); got != "front/app,front/empty,front/flaky,front/huge" {
		t.Errorf("recent extra %s", got)
	}
	now = now.Add(12 * time.Hour)
	_, _, r = p.acquire(context.Background(), core.ContextRequest{Related: p.recentExtra()}, time.Minute, false)
	r()
	now = now.Add(13 * time.Hour)
	if got := p.recentExtra(); len(got) != 0 {
		t.Errorf("warm pass must not keep entries alive: %v", got)
	}

	// Without related, the old behaviour.
	_, missing, r = p.Acquire(context.Background(), core.ContextRequest{})
	r()
	if len(missing) != 0 || paths(s.specs) != "g/a,g/b,g/c" {
		t.Errorf("plain acquire: %s %+v", paths(s.specs), missing)
	}
}

func TestServiceExclude(t *testing.T) {
	l := &fakeLister{data: map[string][]gitlab.ProjectDetails{"g": {
		{PathWithNamespace: "g/a", DefaultBranch: "main"},
		{PathWithNamespace: "g/secret", DefaultBranch: "main"},
	}}}
	s := &fakeSnaps{}
	p := New(Options{Lister: l, Snapshots: s, Groups: []string{"g"}, Exclude: func(path string) bool { return path == "g/secret" }})
	_, missing, release := p.Acquire(context.Background(), core.ContextRequest{Related: []string{"g/secret"}})
	release()
	if paths(s.specs) != "g/a" || len(missing) != 1 || missing[0].Reason != ReasonExcluded {
		t.Fatalf("specs %s missing %+v", paths(s.specs), missing)
	}
}

func TestMainBranchWithFallback(t *testing.T) {
	l := &fakeLister{data: map[string][]gitlab.ProjectDetails{"g": {
		{PathWithNamespace: "g/a", DefaultBranch: "master"},
		{PathWithNamespace: "g/b", DefaultBranch: "main"},
	}}}
	s := &fakeSnaps{failBranch: map[string]bool{"g/b@release": true}}
	p := New(Options{Lister: l, Snapshots: s, Groups: []string{"g"},
		MainBranch: func(path, def string) string { return "release" }})
	dirs, _, release := p.Acquire(context.Background(), core.ContextRequest{})
	defer release()
	got := map[string]string{}
	for _, d := range dirs {
		got[d.Name] = d.Branch
	}
	// g/a has "release"; g/b does not and falls back to its default branch.
	if got["g/a"] != "release" || got["g/b"] != "main" || s.calls != 2 {
		t.Fatalf("branches %v (snapshot calls %d)", got, s.calls)
	}
}
