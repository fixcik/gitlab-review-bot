package diag

import (
	"context"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

type linesFakeRepos struct{ dir string }

func (f linesFakeRepos) Checkout(context.Context, string, *core.MergeRequest) (core.Checkout, error) {
	return linesFakeCheckout{fakeCheckout{dir: f.dir}}, nil
}

type linesFakeCheckout struct{ fakeCheckout }

func (linesFakeCheckout) LinesChanged(context.Context, string, string, string, int, int) (bool, error) {
	return true, nil
}

// The reviewer classifies finding outcomes through the optional
// LinesChanged method of *repo.Checkout (type assertion). The recording
// wrapper must keep it, or every human-resolved finding stays "open"
// forever when diagnostics are on (the default).
func TestWrapReposKeepsLinesChanged(t *testing.T) {
	e := newEnv(t)
	type lc interface {
		LinesChanged(ctx context.Context, from, to, path string, start, end int) (bool, error)
	}
	var got core.Checkout
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
		co, err := e.rec.WrapRepos(linesFakeRepos{dir: t.TempDir()}).Checkout(ctx, j.ProjectPath, &core.MergeRequest{})
		got = co
		return err
	})
	if err := h(context.Background(), core.Job{Kind: core.JobAutoReview, ProjectID: 1, ProjectPath: "g/p", MRIID: 2}); err != nil {
		t.Fatal(err)
	}
	l, ok := got.(lc)
	if !ok {
		t.Fatalf("wrapped checkout %T lost LinesChanged", got)
	}
	if changed, err := l.LinesChanged(context.Background(), "a", "b", "p", 1, 2); err != nil || !changed {
		t.Fatalf("LinesChanged not forwarded: %v %v", changed, err)
	}
	// A checkout without the method must not grow one.
	h2 := e.rec.Handler(func(ctx context.Context, j core.Job) error {
		co, err := e.rec.WrapRepos(fakeRepos{dir: t.TempDir()}).Checkout(ctx, j.ProjectPath, &core.MergeRequest{})
		got = co
		return err
	})
	if err := h2(context.Background(), core.Job{Kind: core.JobAutoReview, ProjectID: 1, ProjectPath: "g/p", MRIID: 3}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(lc); ok {
		t.Fatalf("plain checkout gained LinesChanged")
	}
}
