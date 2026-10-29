package reviewtest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// LinesRepos is FakeRepos whose checkouts also answer LinesChanged.
type LinesRepos struct {
	*FakeRepos
	mu      sync.Mutex
	Changed bool
	err     error
	Calls   []string
}

func (l *LinesRepos) Checkout(ctx context.Context, p string, mr *core.MergeRequest) (core.Checkout, error) {
	co, err := l.FakeRepos.Checkout(ctx, p, mr)
	if err != nil {
		return nil, err
	}
	return &LinesCheckout{Checkout: co, l: l}, nil
}

type LinesCheckout struct {
	core.Checkout
	l *LinesRepos
}

func (c *LinesCheckout) LinesChanged(_ context.Context, from, to, path string, start, end int) (bool, error) {
	c.l.mu.Lock()
	defer c.l.mu.Unlock()
	c.l.Calls = append(c.l.Calls, strings.TrimSpace(fmt.Sprintf("%s %s %d-%d %s", from, path, start, end, to)))
	return c.l.Changed, c.l.err
}

// OutcomesHarness: learn harness (real SQLite) whose publish creates
// threads and whose checkouts can compare lines.
func OutcomesHarness(t *testing.T) (*LearnHarness, *LinesRepos) {
	t.Helper()
	h := NewLearnHarness(t, nil)
	h.GL.PublishMakesThreads = true
	lr := &LinesRepos{FakeRepos: h.Repos}
	h.Deps.Repos = lr
	return h, lr
}

func Outcomes(t *testing.T, h *LearnHarness) []learn.Outcome {
	t.Helper()
	l, err := h.Db.ListOutcomes(context.Background(), learn.OutcomeFilter{ProjectPath: "group/api"})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

var CritFinding = output.Finding{Path: "cache.go", Line: 2, Severity: "critical", Title: "Data loss", Body: "writes are dropped"}

// PostFinding runs a first review that publishes CritFinding as a thread.
func PostFinding(t *testing.T, h *LearnHarness) learn.Outcome {
	t.Helper()
	h.Runner.Resps = append(h.Runner.Resps, RunResp{Out: ReviewOut(t, []output.Finding{CritFinding}, nil)})
	if err := h.R().Handle(context.Background(), AutoJob()); err != nil {
		t.Fatal(err)
	}
	os := Outcomes(t, h)
	if len(os) != 1 {
		t.Fatalf("outcomes %+v", os)
	}
	return os[0]
}
