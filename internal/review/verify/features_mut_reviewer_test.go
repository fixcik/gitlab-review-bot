package verify_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// Mutation-driven tests for the feature files of the package (migrations,
// compat, lint, tbd, coverage, sysmap deps).

// planCompat: deterministic changes and Claude items are de-duplicated by
// id; items already discussed are kept without verification; only new
// Claude items go to ONE verification run, which gets the OpenAPI
// changelog as context.
func TestPlanCompatDedupeAndVerifyMut(t *testing.T) {
	h := reviewtest.New(t)
	openItem := output.BreakingChange{Path: "a.go", Line: 1, Title: "already open"}
	dismissedItem := output.BreakingChange{Path: "a.go", Line: 2, Title: "dismissed"}
	threads := &mrthread.Threads{Open: map[string]mrthread.OpenFinding{}, OpenCompat: map[string]mrthread.OpenCompat{
		compatcheck.ClaudeItem(openItem).ID: {Source: compatcheck.SourceClaude, Path: "a.go", Title: "already open"},
	}, DismissedCompat: map[string]mrthread.OpenCompat{
		compatcheck.ClaudeItem(dismissedItem).ID: {Source: compatcheck.SourceClaude, Path: "a.go", Title: "dismissed"},
	}}
	cr := &compatcheck.Run{Enabled: true, Res: &compat.Result{
		Changes: []compat.Change{reviewtest.OasdiffChange, reviewtest.OasdiffChange},
		API: []compat.APIChanges{{Spec: "openapi.yaml", Items: []compat.APIChange{
			{Level: 3, Rule: "endpoint-removed", Operation: "GET", Endpoint: "/users/{id}", Text: "endpoint removed"}}}},
	}}
	out := &output.ReviewOutput{Compatibility: &output.Compatibility{Breaking: []output.BreakingChange{dropColumn, openItem, dropColumn, dismissedItem}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: verifyOut(t, output.CompatVerdict{Index: 1, Verdict: "confirmed", Reason: "r"})}}
	h.Internals().Verify.PlanCompat(context.Background(), reviewtest.AutoJob(), cr, out, threads, t.TempDir(), "base000000000000", "head111111111111", "Russian")
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("verification runs: %d", len(h.Runner.Reqs))
	}
	p := h.Runner.Reqs[0].Prompt
	if strings.Count(p, "--- item ") != 1 || !strings.Contains(p, dropColumn.Title) || strings.Contains(p, "already open") || strings.Contains(p, "dismissed") {
		t.Errorf("only the new Claude item, once, is verified:\n%s", p)
	}
	if !strings.Contains(p, "<untrusted_api_changes>") || !strings.Contains(p, "/users/{id}") {
		t.Errorf("verification lacks the OpenAPI changelog:\n%s", p)
	}
	var titles []string
	for _, it := range cr.Items {
		titles = append(titles, it.Title)
	}
	if got := strings.Join(titles, "|"); got != reviewtest.OasdiffChange.Message+"|already open|dismissed|"+dropColumn.Title {
		t.Errorf("items %s", got)
	}

	// Without Claude's section only the deterministic changes count and
	// nothing is verified.
	h = reviewtest.New(t)
	cr = &compatcheck.Run{Enabled: true, Res: &compat.Result{Changes: []compat.Change{reviewtest.OasdiffChange}}}
	h.Internals().Verify.PlanCompat(context.Background(), reviewtest.AutoJob(), cr, &output.ReviewOutput{}, threads, t.TempDir(), "b", "h", "Russian")
	if len(cr.Items) != 1 || len(h.Runner.Reqs) != 0 {
		t.Errorf("items %+v runs %d", cr.Items, len(h.Runner.Reqs))
	}
	// Deterministic check failed: Claude items are still verified, without
	// an OpenAPI changelog.
	h = reviewtest.New(t)
	cr = &compatcheck.Run{Enabled: true, Err: context.DeadlineExceeded}
	h.Runner.Resps = []reviewtest.RunResp{{Out: verifyOut(t, output.CompatVerdict{Index: 1, Verdict: "confirmed", Reason: "r"})}}
	h.Internals().Verify.PlanCompat(context.Background(), reviewtest.AutoJob(), cr, &output.ReviewOutput{Compatibility: &output.Compatibility{Breaking: []output.BreakingChange{dropColumn}}},
		threads, t.TempDir(), "b", "h", "Russian")
	if len(cr.Items) != 1 || len(h.Runner.Reqs) != 1 || strings.Contains(h.Runner.Reqs[0].Prompt, "untrusted_api_changes") {
		t.Errorf("items %+v runs %d", cr.Items, len(h.Runner.Reqs))
	}
}
