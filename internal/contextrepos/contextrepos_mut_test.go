package contextrepos

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
)

// A group whose listing fails does not hide the groups after it.
func TestFailingFirstGroupDoesNotStopListing(t *testing.T) {
	l := &fakeLister{
		data: map[string][]gitlab.ProjectDetails{"b": {{PathWithNamespace: "b/x", DefaultBranch: "main"}}},
		fail: map[string]bool{"a": true},
	}
	s := &fakeSnaps{}
	p := New(Options{Lister: l, Snapshots: s, Groups: []string{"a", "b"}})
	_, _, release := p.Acquire(context.Background(), core.ContextRequest{})
	release()
	if got := paths(s.specs); got != "b/x" {
		t.Fatalf("specs %q", got)
	}
}

// One repository without a snapshot does not drop the ones after it.
func TestMissingSnapshotKeepsTheRest(t *testing.T) {
	l := &fakeLister{data: map[string][]gitlab.ProjectDetails{"g": {
		{PathWithNamespace: "g/a", DefaultBranch: "main"},
		{PathWithNamespace: "g/b", DefaultBranch: "main"},
		{PathWithNamespace: "g/c", DefaultBranch: "main"},
	}}}
	s := &fakeSnaps{fail: map[string]bool{"g/b": true}}
	p := New(Options{Lister: l, Snapshots: s, Groups: []string{"g"}})
	dirs, missing, release := p.Acquire(context.Background(), core.ContextRequest{})
	defer release()
	var names []string
	for _, d := range dirs {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "g/a,g/c" || len(missing) != 0 {
		t.Fatalf("dirs %v missing %+v", names, missing)
	}
}

// max_repo_mb: a repository of exactly the limit is used.
func TestMaxRepoBytesBoundary(t *testing.T) {
	l := &fakeLister{data: map[string][]gitlab.ProjectDetails{"g": {
		{PathWithNamespace: "g/exact", DefaultBranch: "main", RepositorySize: 1 << 20},
		{PathWithNamespace: "g/over", DefaultBranch: "main", RepositorySize: 1<<20 + 1},
	}}}
	s := &fakeSnaps{}
	p := New(Options{Lister: l, Snapshots: s, Groups: []string{"g"}, MaxRepoBytes: 1 << 20})
	_, _, release := p.Acquire(context.Background(), core.ContextRequest{})
	release()
	if got := paths(s.specs); got != "g/exact" {
		t.Fatalf("specs %q", got)
	}
	// 0 = no limit.
	s = &fakeSnaps{}
	p = New(Options{Lister: l, Snapshots: s, Groups: []string{"g"}})
	_, _, release = p.Acquire(context.Background(), core.ContextRequest{})
	release()
	if got := paths(s.specs); got != "g/exact,g/over" {
		t.Fatalf("no limit: specs %q", got)
	}
}

// Lookups of projects outside the groups: a success is cached for
// RefreshInterval, a failure only for a minute; a cancelled lookup is not
// cached at all and reported as a GitLab error, not "not found".
func TestLookupExtraCaching(t *testing.T) {
	l := &fakeLister{single: map[string]*gitlab.ProjectDetails{}}
	s := &fakeSnaps{}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := New(Options{Lister: l, Snapshots: s, RefreshInterval: time.Hour, Now: func() time.Time { return now }})
	req := core.ContextRequest{Related: []string{"x/late"}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, missing, r := p.Acquire(ctx, req)
	r()
	if len(missing) != 1 || missing[0].Reason != ReasonLookupErr {
		t.Fatalf("cancelled lookup: %+v", missing)
	}
	_, missing, r = p.Acquire(context.Background(), req)
	r()
	if len(missing) != 1 || missing[0].Reason != ReasonNotFound || l.lookups != 2 {
		t.Fatalf("cancelled lookup was cached: %+v lookups=%d", missing, l.lookups)
	}

	// Access granted: seen after the one-minute failure TTL, well within
	// RefreshInterval.
	l.single["x/late"] = &gitlab.ProjectDetails{PathWithNamespace: "x/late", DefaultBranch: "main"}
	now = now.Add(2 * time.Minute)
	dirs, missing, r := p.Acquire(context.Background(), req)
	r()
	if len(dirs) != 1 || len(missing) != 0 {
		t.Fatalf("failure cached too long: dirs %+v missing %+v", dirs, missing)
	}
	// Success is cached for RefreshInterval (even if access is lost).
	delete(l.single, "x/late")
	now = now.Add(30 * time.Minute)
	dirs, _, r = p.Acquire(context.Background(), req)
	r()
	if len(dirs) != 1 || l.lookups != 3 {
		t.Fatalf("success not cached: dirs %+v lookups=%d", dirs, l.lookups)
	}
}

// A real run that hits the cache still counts as a use for the warm set.
func TestCachedLookupKeepsProjectWarm(t *testing.T) {
	l := &fakeLister{single: map[string]*gitlab.ProjectDetails{
		"x/app": {PathWithNamespace: "x/app", DefaultBranch: "main"},
	}}
	s := &fakeSnaps{}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := New(Options{Lister: l, Snapshots: s, RefreshInterval: 48 * time.Hour, Now: func() time.Time { return now }})
	req := core.ContextRequest{Related: []string{"x/app"}}
	_, _, r := p.Acquire(context.Background(), req)
	r()
	now = now.Add(20 * time.Hour)
	_, _, r = p.Acquire(context.Background(), req) // cache hit
	r()
	if l.lookups != 1 {
		t.Fatalf("expected a cache hit, lookups=%d", l.lookups)
	}
	now = now.Add(10 * time.Hour) // 30h after the first use, 10h after the last
	if got := strings.Join(p.recentExtra(), ","); got != "x/app" {
		t.Fatalf("recently used project dropped from the warm set: %q", got)
	}
}
