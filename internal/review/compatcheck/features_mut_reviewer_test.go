package compatcheck_test

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

// resolveFixedCompat: a Claude thread closes only when Claude lists it as
// resolved AND does not report it again; a deterministic thread closes only
// when its source ran and no longer reports it. Every thread is looked at,
// whatever comes before it.
func TestResolveFixedCompatMut(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.WithModerator(h)
	again := output.BreakingChange{Path: "a.go", Title: "still broken"}
	fixed := output.BreakingChange{Path: "a.go", Title: "fixed"}
	notListed := output.BreakingChange{Path: "a.go", Title: "not listed"}
	idAgain, idFixed, idNot := compatcheck.ClaudeItem(again).ID, compatcheck.ClaudeItem(fixed).ID, compatcheck.ClaudeItem(notListed).ID
	det := compatcheck.DeterministicItem(reviewtest.OasdiffChange)
	threads := &mrthread.Threads{Open: map[string]mrthread.OpenFinding{}, DismissedCompat: map[string]mrthread.OpenCompat{}, OpenCompat: map[string]mrthread.OpenCompat{
		idAgain: {DiscussionID: "d-again", Source: compatcheck.SourceClaude},
		idFixed: {DiscussionID: "d-fixed", Source: compatcheck.SourceClaude},
		idNot:   {DiscussionID: "d-not", Source: compatcheck.SourceClaude},
		det.ID:  {DiscussionID: "d-det", Source: compat.SourceOasdiff},
	}, CompatOrder: []string{"gone", idAgain, idNot, idFixed, det.ID}}
	cr := &compatcheck.Run{Enabled: true, Res: &compat.Result{}, Items: []compatcheck.Item{compatcheck.ClaudeItem(again)}}
	out := &output.ReviewOutput{Compatibility: &output.Compatibility{Resolved: []output.ResolvedFinding{
		{ID: idAgain, Comment: "c"}, {ID: idFixed, Comment: "c"},
	}}}
	h.Internals().Compat.ResolveFixed(context.Background(), reviewtest.AutoJob(), cr, out, threads)
	got := strings.Join(m.Resolved, ",")
	if got != "d-fixed,d-det" || cr.Closed != 2 {
		t.Errorf("resolved %q, closed %d", got, cr.Closed)
	}
	var rem []string
	for _, oc := range cr.Remaining {
		rem = append(rem, oc.DiscussionID)
	}
	if r := strings.Join(rem, ","); !strings.HasPrefix(r, "d-again,d-not") {
		t.Errorf("remaining %q", r)
	}
	if _, ok := threads.OpenCompat[idFixed]; ok {
		t.Error("closed thread still open")
	}

	// The deterministic source did not run: its thread stays open.
	h = reviewtest.New(t)
	m = reviewtest.WithModerator(h)
	threads = &mrthread.Threads{Open: map[string]mrthread.OpenFinding{}, DismissedCompat: map[string]mrthread.OpenCompat{}, OpenCompat: map[string]mrthread.OpenCompat{
		det.ID: {DiscussionID: "d-det", Source: compat.SourceOasdiff},
	}, CompatOrder: []string{det.ID}}
	cr = &compatcheck.Run{Enabled: true, Res: &compat.Result{Issues: []compat.Issue{{Source: compat.SourceOasdiff, Kind: compat.IssueUnavailable}}}}
	h.Internals().Compat.ResolveFixed(context.Background(), reviewtest.AutoJob(), cr, &output.ReviewOutput{Compatibility: &output.Compatibility{}}, threads)
	if len(m.Resolved) != 0 || len(cr.Remaining) != 1 {
		t.Errorf("resolved %v remaining %v", m.Resolved, cr.Remaining)
	}

	// A thread of the removed compat.commands is never auto-resolved: the
	// check no longer runs, so its silence proves nothing.
	h = reviewtest.New(t)
	m = reviewtest.WithModerator(h)
	threads = &mrthread.Threads{Open: map[string]mrthread.OpenFinding{}, DismissedCompat: map[string]mrthread.OpenCompat{}, OpenCompat: map[string]mrthread.OpenCompat{
		"custom-id": {DiscussionID: "d-custom", Source: compat.SourceCustom},
	}, CompatOrder: []string{"custom-id"}}
	cr = &compatcheck.Run{Enabled: true, Res: &compat.Result{}}
	h.Internals().Compat.ResolveFixed(context.Background(), reviewtest.AutoJob(), cr, &output.ReviewOutput{Compatibility: &output.Compatibility{}}, threads)
	if len(m.Resolved) != 0 || len(cr.Remaining) != 1 || cr.Closed != 0 {
		t.Errorf("custom: resolved %v remaining %v", m.Resolved, cr.Remaining)
	}
}
