package review_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// gitDiffAddRemove is a `git diff` of one file with added and removed lines.
func gitDiffAddRemove(path string, added, removed int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%[1]s b/%[1]s\n--- a/%[1]s\n+++ b/%[1]s\n@@ -1,%[2]d +1,%[3]d @@\n", path, removed, added)
	for i := 0; i < removed; i++ {
		fmt.Fprintf(&b, "-old %d\n", i)
	}
	for i := 0; i < added; i++ {
		fmt.Fprintf(&b, "+new %d\n", i)
	}
	return b.String()
}

// The size limit counts added and removed lines; a range of exactly the
// limit is reviewed.
func TestSizeLimitCountsBothSidesAndBoundary(t *testing.T) {
	for _, c := range []struct {
		name           string
		added, removed int
		reviewed       bool
	}{
		{"removed lines count", 1500, 600, false},
		{"exactly the limit", 1400, 600, true},
		{"one over", 1401, 600, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := reviewtest.NewBigHarness(t, 10)
			h.Repos.Diff = gitDiffAddRemove("gen/schema.go", c.added, c.removed)
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if got := len(h.Runner.Reqs) == 1; got != c.reviewed {
				t.Fatalf("reviewed %v, want %v", got, c.reviewed)
			}
			if !c.reviewed {
				n := c.added + c.removed
				want := fmt.Sprintf("%d %s (порог 2000)", n, mdtext.LinesWord(n))
				if !strings.Contains(h.Summary(), want) {
					t.Fatalf("summary lacks %q:\n%s", want, h.Summary())
				}
			}
		})
	}
}

// The deterministic compat checks of a too-big MR publish their breaking
// changes inline (the diff index is built for them) and they block the
// merge; a secret found by gitleaks is anchored too.
func TestTooBigDeterministicThreadsAnchoredAndBlocking(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Compat.Res = &compat.Result{Changes: []compat.Change{reviewtest.OasdiffChange}}
	h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	ct := reviewtest.CompatThreads(h.Harness)
	if len(ct) != 1 || ct[0].Pos == nil || ct[0].Pos.NewLine != 3 {
		t.Fatalf("compat threads %+v", ct)
	}
	sum := h.Summary()
	if !strings.HasPrefix(sum, marker.Summary("head111111111111")+"\n"+summary.Title) || !strings.Contains(sum, summary.GateLine(1, 0)) {
		t.Fatalf("summary:\n%s", sum)
	}

	h2 := reviewtest.NewBigHarness(t, 2100)
	h2.Compat.Res = &compat.Result{}
	h2.Fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.SecretFinding}, Tools: reviewtest.Ran(lint.ToolGitleaks)}
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	lt := reviewtest.LintThreads(h2.Harness)
	if len(lt) != 1 || lt[0].Pos == nil || lt[0].Pos.NewLine != 2 {
		t.Fatalf("secret threads %+v", lt)
	}
}

// Every kind of open blocking bot thread counts in the gate of a too-big
// MR: other blocking threads and threads the bot declared fixed but could
// not resolve (pending).
func TestTooBigGateCountsOtherAndPending(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
	pending := chkFindingDisc("f1", chkDataLoss, true)
	pending.Notes = append(pending.Notes, reviewtest.BotNote(901, marker.Fixed+"\n✅ Похоже, исправлено.", nil))
	h.GL.Discussions = []core.Discussion{
		{ID: "other", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(601, "<!-- reviewbot:future-kind -->\n"+marker.Blocking+"\nx", nil)}},
		pending,
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if sum := h.Summary(); !strings.Contains(sum, summary.GateLine(2, 1)) {
		t.Fatalf("gate:\n%s", sum)
	}
}

// What the too-big run could not check is announced once: the people to
// notify are marked notified when the summary went out, not when it failed.
func TestTooBigProblemsNotified(t *testing.T) {
	for _, writeFails := range []bool{false, true} {
		h := reviewtest.NewBigHarness(t, 2100)
		h.Cfg.Review.Lint.Enabled = true
		h.Fl.Res = &lint.Result{Tools: []lint.ToolStatus{{Tool: lint.ToolGitleaks, State: lint.StateFailed, Reason: "панический выход"}}}
		if writeFails {
			gl, _, _ := reviewtest.WithFaults(h.Harness)
			gl.Fail = reviewtest.FailOn(errBoom, "CreateNote")
		}
		ps := &reviewtest.ProbStore{Rows: map[string]*problems.Record{}}
		svc := problems.New(problems.Options{Store: ps, Notify: []string{"alice"}, Now: func() time.Time { return reviewtest.TestNow }})
		if err := svc.Handler(h.R().Handle)(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		if len(ps.Rows) == 0 {
			t.Fatal("no problem recorded")
		}
		for _, r := range ps.Rows {
			if notified := r.NotifiedAt.Equal(reviewtest.TestNow); notified == writeFails {
				t.Fatalf("summary write fails %v: notified %v", writeFails, notified)
			}
		}
		if !writeFails && !strings.Contains(h.Summary(), "cc @alice") {
			t.Fatalf("summary:\n%s", h.Summary())
		}
	}
}

// After a review of a shrunk MR only an open size thread is closed: one a
// person resolved, or one the bot already declared fixed, gets no reply.
func TestShrunkMRLeavesClosedSizeThreads(t *testing.T) {
	for name, d := range map[string]core.Discussion{
		"resolved by a person": {ID: "size", Resolvable: true, Resolved: true,
			Notes: []core.Note{reviewtest.BotNote(600, marker.TooBigThread+"\n"+marker.Blocking+"\n📦 big", nil)}},
		"declared fixed": {ID: "size", Resolvable: true,
			Notes: []core.Note{reviewtest.BotNote(600, marker.TooBigThread+"\n"+marker.Blocking+"\n📦 big", nil),
				reviewtest.BotNote(601, marker.Fixed+"\n✅ Похоже, исправлено.", nil)}},
	} {
		t.Run(name, func(t *testing.T) {
			h := reviewtest.NewBigHarness(t, 10)
			h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
			h.GL.Discussions = []core.Discussion{d}
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if len(repliesTo(h.GL, "size"))+len(h.GL.Resolved) != 0 {
				t.Fatalf("replies %+v resolved %v", h.GL.Replies, h.GL.Resolved)
			}
		})
	}
}

// The size thread of a reverted big change is resolved: it no longer counts,
// another open blocking thread still does (not as pending).
func TestNothingToReviewResolvedSizeThreadRecount(t *testing.T) {
	h := nothingToReview(t)
	reviewtest.WithModerator(h.Harness)
	h.GL.Discussions = append(h.GL.Discussions, core.Discussion{ID: "other", Resolvable: true,
		Notes: []core.Note{reviewtest.BotNote(601, "<!-- reviewbot:future-kind -->\n"+marker.Blocking+"\nx", nil)}})
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if sum := h.GL.Notes[500]; !strings.Contains(sum, summary.GateLine(1, 0)) {
		t.Fatalf("gate:\n%s", sum)
	}
}

func TestConfigCommandShowsLimitWithoutLabel(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Review.BigMR.OverrideLabel = ""
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobConfig)); err != nil {
		t.Fatal(err)
	}
	if body := reviewtest.LastReply(t, h.GL).Body; !strings.Contains(body, "| порог размера MR (`max_mr_lines`) | 2000 строк | по умолчанию |") {
		t.Fatalf("config:\n%s", body)
	}
}
