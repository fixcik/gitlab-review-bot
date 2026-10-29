package review_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/jira"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
)

// analyzerDelay is how long each slow fake analyzer takes.
const analyzerDelay = 150 * time.Millisecond

// slowAnalyzers records which slow fakes ran and how many overlapped.
type slowAnalyzers struct {
	mu      sync.Mutex
	ran     []string
	running atomic.Int32
	peak    atomic.Int32
}

func (s *slowAnalyzers) work(name string) {
	n := s.running.Add(1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(analyzerDelay)
	s.running.Add(-1)
	s.mu.Lock()
	s.ran = append(s.ran, name)
	s.mu.Unlock()
}

type slowCompat struct {
	compatcheck.Checker
	s *slowAnalyzers
}

func (c slowCompat) Check(ctx context.Context, req compat.Request) (*compat.Result, error) {
	c.s.work("compat")
	return c.Checker.Check(ctx, req)
}

type slowLint struct {
	linters.Checker
	s *slowAnalyzers
}

func (l slowLint) Check(ctx context.Context, req lint.Request) (*lint.Result, error) {
	l.s.work("lint")
	return l.Checker.Check(ctx, req)
}

type slowTasks struct {
	task.Source
	s *slowAnalyzers
}

func (t slowTasks) GetIssue(ctx context.Context, key string) (*jira.Issue, error) {
	t.s.work("task")
	return t.Source.GetIssue(ctx, key)
}

type slowRepos struct {
	core.Repos
	s *slowAnalyzers
}

func (r slowRepos) Checkout(ctx context.Context, p string, mr *core.MergeRequest) (core.Checkout, error) {
	co, err := r.Repos.Checkout(ctx, p, mr)
	if err != nil {
		return nil, err
	}
	return &slowCheckout{Checkout: co, s: r.s}, nil
}

// slowCheckout makes the merge base export slow.
type slowCheckout struct {
	core.Checkout
	s *slowAnalyzers
}

func (c *slowCheckout) Unwrap() core.Checkout { return c.Checkout }

func (c *slowCheckout) ExportTree(ctx context.Context, rev string, max int64) (string, func(), error) {
	c.s.work("base")
	ex, _ := core.CheckoutAs[core.TreeExporter](c.Checkout)
	return ex.ExportTree(ctx, rev, max)
}

// slowHarness is a harness whose base export, compat, lint and Jira steps
// each take analyzerDelay.
func slowHarness(t *testing.T, parallel bool) (*reviewtest.Harness, *slowAnalyzers) {
	h := reviewtest.New(t)
	s := &slowAnalyzers{}
	ft := &reviewtest.FakeTasks{Issues: map[string]*jira.Issue{"PROJ-1": reviewtest.CacheIssue()}}
	reviewtest.WithJira(h, ft)
	h.Cfg.Review.ParallelAnalyzers = parallel
	h.Deps.Repos = slowRepos{Repos: h.Repos, s: s}
	h.Deps.Compat = slowCompat{Checker: h.Compat, s: s}
	h.Deps.Lint = slowLint{Checker: &reviewtest.FakeLint{}, s: s}
	h.Deps.Tasks = slowTasks{Source: ft, s: s}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	return h, s
}

func TestAnalyzersRunInParallel(t *testing.T) {
	h, s := slowHarness(t, true)
	start := time.Now()
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	s.mu.Lock()
	ran := strings.Join(s.ran, ",")
	s.mu.Unlock()
	if len(s.ran) != 4 {
		t.Fatalf("slow analyzers that ran: %s", ran)
	}
	sum := 4 * analyzerDelay
	if took >= sum*3/4 {
		t.Fatalf("parallel analyzers took %s, the sum of the steps is %s", took, sum)
	}
	if s.peak.Load() < 2 {
		t.Fatalf("no overlap: peak %d", s.peak.Load())
	}
	if len(h.Runner.Reqs) != 1 || h.Runner.Reqs[0].BaseDir == "" {
		t.Fatalf("the review did not run with the results: %+v", h.Runner.Reqs)
	}
	t.Logf("parallel: %s for %s of steps (%s), peak %d", took, sum, ran, s.peak.Load())
}

func TestAnalyzersSequentialSwitch(t *testing.T) {
	h, s := slowHarness(t, false)
	start := time.Now()
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if len(s.ran) != 4 || s.peak.Load() != 1 {
		t.Fatalf("sequential: ran %v, peak %d", s.ran, s.peak.Load())
	}
	// The sequential order of the flow.
	if got := strings.Join(s.ran, ","); got != "base,compat,lint,task" {
		t.Fatalf("order: %s", got)
	}
	if took < 4*analyzerDelay {
		t.Fatalf("sequential took %s", took)
	}
}
