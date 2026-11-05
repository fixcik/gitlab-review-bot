package review_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

func conflictHarness(t *testing.T, status string, conflicts bool) (*reviewtest.Harness, *[]string) {
	t.Helper()
	restore := review.SetMergeStatusWaits(0, 0)
	t.Cleanup(restore)
	h := reviewtest.New(t)
	h.GL.MR.TargetBranch = "dev"
	h.GL.MR.DivergedCommits = 136
	h.GL.MR.MergeStatus, h.GL.MR.HasConflicts = status, conflicts
	var skips []string
	h.Deps.OnSkip = func(reason string) { skips = append(skips, reason) }
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	return h, &skips
}

func summaryOf(h *reviewtest.Harness) string {
	h.GL.Mu.Lock()
	defer h.GL.Mu.Unlock()
	st, _ := h.St.Get()
	return h.GL.Notes[st.SummaryNoteID]
}

func TestConflictSkipsClaudeAndWritesBlock(t *testing.T) {
	h, skips := conflictHarness(t, "conflict", true)
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || h.Repos.Closed != 0 {
		t.Fatalf("claude or checkout ran: %d runs, %d closed", len(h.Runner.Reqs), h.Repos.Closed)
	}
	if len(*skips) != 1 || (*skips)[0] != review.SkipConflict {
		t.Fatalf("skip metric: %v", *skips)
	}
	sum := summaryOf(h)
	for _, want := range []string{marker.ConflictStart, "Конфликт слияния с `dev`", "отстаёт от `dev` на 136 коммитов", "review force"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
	// The head stays unreviewed: the commit that resolves the conflict starts a full review.
	if st, _ := h.St.Get(); st.ReviewedSHA != "" || st.SummaryNoteID == 0 {
		t.Fatalf("state: %+v", st)
	}
	// A second run of the same head changes nothing (no note churn): GitLab lists
	// the summary note among the discussions.
	h.GL.Mu.Lock()
	st, _ := h.St.Get()
	h.GL.Discussions = []core.Discussion{{ID: "summary", Notes: []core.Note{reviewtest.BotNote(st.SummaryNoteID, sum, nil)}}}
	h.GL.Mu.Unlock()
	n := len(h.GL.UpdatedNotes) + len(h.GL.CreatedNotes)
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := len(h.GL.UpdatedNotes) + len(h.GL.CreatedNotes); got != n {
		t.Fatalf("summary rewritten without a change: %d -> %d", n, got)
	}
}

func TestNoConflictReviewsAsUsual(t *testing.T) {
	for _, tc := range []struct {
		name, status string
	}{{"mergeable", "mergeable"}, {"not reported", ""}, {"other reason", "discussions_not_resolved"}} {
		t.Run(tc.name, func(t *testing.T) {
			h, skips := conflictHarness(t, tc.status, false)
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if len(h.Runner.Reqs) != 1 || len(*skips) != 0 {
				t.Fatalf("runs %d skips %v", len(h.Runner.Reqs), *skips)
			}
			if strings.Contains(summaryOf(h), marker.ConflictStart) {
				t.Fatal("conflict block without a conflict")
			}
		})
	}
}

func TestMergeStatusUnknownReviewsAsUsual(t *testing.T) {
	// GitLab never settles ("checking" on every read): never skip on a guess.
	h, skips := conflictHarness(t, "checking", true)
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(*skips) != 0 {
		t.Fatalf("runs %d skips %v", len(h.Runner.Reqs), *skips)
	}
}

func TestMergeStatusSettlesToConflict(t *testing.T) {
	h, skips := conflictHarness(t, "checking", false)
	settled := h.GL.MR
	settled.MergeStatus, settled.HasConflicts = "conflict", true
	// 1st read: preflight (checking); 2nd: the first re-read (still checking); 3rd: settled.
	h.GL.MRReads = []core.MergeRequest{h.GL.MR, h.GL.MR, settled}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || len(*skips) != 1 {
		t.Fatalf("runs %d skips %v", len(h.Runner.Reqs), *skips)
	}
}

func TestForceReviewIgnoresConflict(t *testing.T) {
	h, skips := conflictHarness(t, "conflict", true)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobForceReview)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(*skips) != 0 {
		t.Fatalf("runs %d skips %v", len(h.Runner.Reqs), *skips)
	}
}

func TestManualReviewOfConflictReplies(t *testing.T) {
	h, _ := conflictHarness(t, "conflict", true)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 {
		t.Fatalf("claude ran on a conflicted MR")
	}
	found := false
	for _, r := range h.GL.Replies {
		found = found || strings.Contains(r.Body, "конфликт слияния с `dev`")
	}
	if !found {
		t.Fatalf("no reply: %+v", h.GL.Replies)
	}
}

var _ = output.Finding{}

func TestConflictKeepsEarlierSummary(t *testing.T) {
	h, _ := conflictHarness(t, "conflict", true)
	// An earlier review wrote this summary and reviewed another head.
	old := marker.Summary("old000000000000") + "\n## 🤖 Ревью\n\nНашёл 2 замечания.\n\n" + summary.GateClearLine + "\n\n" + marker.TBDStart + "\ntbd\n" + marker.TBDEnd + "\n"
	h.GL.Notes = map[int64]string{900: old}
	h.GL.Discussions = []core.Discussion{{ID: "summary", Notes: []core.Note{reviewtest.BotNote(900, old, nil)}}}
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old000000000000", SummaryNoteID: 900}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	got := h.GL.Notes[900]
	for _, want := range []string{"Нашёл 2 замечания.", marker.TBDStart, marker.ConflictStart, marker.Summary("old000000000000")} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, marker.ConflictStart) > strings.Index(got, "Нашёл 2 замечания.") {
		t.Errorf("the block must come right under the title:\n%s", got)
	}
	// "No blocking threads" must not read as approval of the unreviewed head, and a
	// later gate refresh must not bring it back.
	if strings.Contains(got, summary.GateClearLine) || !strings.Contains(got, summary.GateUnreviewedLine) {
		t.Errorf("gate line:\n%s", got)
	}
	if again, ok := summary.SetGateLine(got, summary.GateClearLine); !ok || again != got {
		t.Errorf("a gate refresh lifted the unreviewed line:\n%s", again)
	}
	if st, _ := h.St.Get(); st.ReviewedSHA != "old000000000000" {
		t.Fatalf("the reviewed head moved: %+v", st)
	}
}

func TestHeadChangedWhileWaitingReviewsAsUsual(t *testing.T) {
	h, skips := conflictHarness(t, "checking", false)
	other := h.GL.MR
	other.MergeStatus, other.HasConflicts = "conflict", true
	other.DiffRefs.HeadSHA = "newhead00000000" // a push arrived: that status is of another head
	h.GL.MRReads = []core.MergeRequest{h.GL.MR, other}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(*skips) != 0 {
		t.Fatalf("runs %d skips %v", len(h.Runner.Reqs), *skips)
	}
}

func TestConflictSummaryWriteFailureFailsTheJob(t *testing.T) {
	h, _ := conflictHarness(t, "conflict", true)
	h.GL.NoteErr = errors.New("gitlab is down")
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err == nil {
		t.Fatal("the job must fail so that it is retried")
	}
	if st, _ := h.St.Get(); st.SummaryNoteID != 0 {
		t.Fatalf("state saved without a summary: %+v", st)
	}
}
