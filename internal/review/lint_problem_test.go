package review_test

import (
	"context"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestLintProblemIsFixedWhenTheToolRuns(t *testing.T) {
	store := &reviewtest.ProbStore{Rows: map[string]*problems.Record{}}
	svc := problems.New(problems.Options{Store: store, Now: func() time.Time { return reviewtest.TestNow }})
	review := func(tools []lint.ToolStatus) {
		ctx, run := svc.Start(context.Background(), reviewtest.AutoJob())
		linters.NoteProblems(ctx, &linters.Run{Res: &lint.Result{Tools: tools}})
		problems.Commit(ctx)
		svc.Finish(ctx, run)
	}
	active := func() (open, fixed int) {
		for _, r := range store.Rows {
			if r.ResolvedAt.IsZero() {
				open++
			} else {
				fixed++
			}
		}
		return open, fixed
	}

	review([]lint.ToolStatus{{Tool: lint.ToolGitleaks, State: lint.StateFailed, Reason: "boom"}})
	if o, f := active(); o != 1 || f != 0 {
		t.Fatalf("after a failure: open %d fixed %d", o, f)
	}
	// A tool that did not run (not applicable) says nothing about the failure.
	review([]lint.ToolStatus{{Tool: lint.ToolGitleaks, State: lint.StateNotApplicable}})
	if o, f := active(); o != 1 || f != 0 {
		t.Fatalf("n/a must not fix it: open %d fixed %d", o, f)
	}
	// The next review ran it: the problem is fixed.
	review(reviewtest.Ran(lint.ToolGitleaks))
	if o, f := active(); o != 0 || f != 1 {
		t.Fatalf("after a good run: open %d fixed %d", o, f)
	}
	// Failing again reopens the same row.
	review([]lint.ToolStatus{{Tool: lint.ToolGitleaks, State: lint.StateFailed, Reason: "boom"}})
	if o, f := active(); o != 1 || f != 0 {
		t.Fatalf("after a new failure: open %d fixed %d", o, f)
	}
}
