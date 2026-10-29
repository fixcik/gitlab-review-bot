package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// The gate of a full review counts every open blocking bot thread it does
// not decide on itself: other blocking threads, and the blocking lint
// threads of earlier reviews while lint is off.
func TestReviewGateCountsOtherAndLintThreadsWhileLintOff(t *testing.T) {
	for _, todo := range []bool{true, false} { // todo off too: no lint run at all
		h := reviewtest.New(t)
		h.Cfg.Review.Lint.Enabled = false
		h.Cfg.Review.Todo.Enabled = todo
		h.Deps.Lint = &reviewtest.FakeLint{}
		h.GL.Discussions = []core.Discussion{
			{ID: "other", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(601, "<!-- reviewbot:future-kind -->\n"+marker.Blocking+"\nx", nil)}},
			{ID: "l1", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(602, marker.Lint("ccc", "gitleaks")+"\n"+marker.Blocking+"\nsecret", nil)}},
		}
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		st, _ := h.St.Get()
		if sum := h.GL.Notes[st.SummaryNoteID]; !strings.Contains(sum, summary.GateLine(2, 0)) {
			t.Fatalf("todo %v: gate:\n%s", todo, sum)
		}
	}
}

// Claude's resolved list is walked to the end: an id that is not an open
// finding, or a thread that cannot be resolved, does not stop the others.
func TestReviewClosesEveryResolvedFinding(t *testing.T) {
	for name, c := range map[string]struct {
		failResolve string
		resolved    []output.ResolvedFinding
		want        string
		gate        string
	}{
		"unknown id first": {resolved: []output.ResolvedFinding{{ID: "ffffffffffff", Comment: "?"}, {ID: chkID(chkDataLoss), Comment: "ok"}},
			want: "f1", gate: summary.GateLine(1, 0)},
		"failing resolve first": {failResolve: "f1", resolved: []output.ResolvedFinding{{ID: chkID(chkDataLoss), Comment: "ok"}, {ID: chkID(chkRace), Comment: "ok"}},
			want: "f3", gate: summary.GateLine(1, 0)},
	} {
		t.Run(name, func(t *testing.T) {
			h := reviewtest.New(t)
			h.GL.Discussions = []core.Discussion{chkFindingDisc("f1", chkDataLoss, true), chkFindingDisc("f3", chkRace, true)}
			h.GL.FailResolveFor = c.failResolve
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, c.resolved)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(h.GL.Resolved, ","); got != c.want {
				t.Fatalf("resolved %q, want %q", got, c.want)
			}
			st, _ := h.St.Get()
			if sum := h.GL.Notes[st.SummaryNoteID]; !strings.Contains(sum, c.gate) {
				t.Fatalf("gate:\n%s", sum)
			}
		})
	}
}

// A postponed status in a fresh note carries the reviewed head; a known
// summary note is updated in place without saving the state again.
func TestPostponedStatusMarkerAndState(t *testing.T) {
	h := reviewtest.New(t)
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "abc123", SummaryNoteID: 0}
	h.Repos.Ancestors["abc123"] = true
	postponedAuto(t, h)
	if len(h.GL.CreatedNotes) != 1 || !strings.HasPrefix(h.GL.Notes[h.GL.CreatedNotes[0]], marker.Summary("abc123")+"\n") {
		t.Fatalf("notes %v", h.GL.Notes)
	}

	h2 := reviewtest.New(t)
	h2.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "abc123", SummaryNoteID: 500}
	h2.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(500, marker.Summary("abc123")+"\n"+summary.Title+"\n\nстарое\n", nil)}}}
	h2.Repos.Ancestors["abc123"] = true
	_, st, _ := reviewtest.WithFaults(h2)
	postponedAuto(t, h2)
	if st.Calls("SaveMRState") != 0 || len(h2.GL.UpdatedNotes) != 1 {
		t.Fatalf("saves %d updates %v", st.Calls("SaveMRState"), h2.GL.UpdatedNotes)
	}
}

// A too-big MR without a stored state whose summary (found by its marker)
// cannot be updated: no state is saved for a note that was not written.
func TestTooBigSummaryUpdateErrorSavesNoState(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(500, marker.Summary("abc123")+"\n"+summary.Title+"\n", nil)}}}
	gl, _, _ := reviewtest.WithFaults(h.Harness)
	gl.Fail = reviewtest.FailOn(errBoom, "UpdateNote")
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if gl.Calls("UpdateNote") != 1 {
		t.Fatalf("updates %d", gl.Calls("UpdateNote"))
	}
	if st, ok := h.St.Get(); ok {
		t.Fatalf("state %+v", st)
	}
}

// A mode change is never trivial, even with a whitespace-only edit; neither
// is a file that cannot be read at the reviewed head.
func TestTrivialIncrementHeaderAndUnreadableOld(t *testing.T) {
	for name, c := range map[string]struct {
		diff          string
		before, after map[string]string
	}{
		"mode change": {"diff --git a/cache.go b/cache.go\nold mode 100644\nnew mode 100755\nindex 1111111..2222222\n--- a/cache.go\n+++ b/cache.go\n@@ -1 +1 @@\n-x := 1\n+x  :=  1\n",
			trivWSBefore, trivWSAfter},
		"old version unreadable, blank new one": {fileDiff("cache.go", "@@ -1 +1 @@\n-x\n+  \n"), nil, map[string]string{"cache.go": "  \n"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := trivialHarness(t, c.diff, []string{"cache.go"}, c.before, c.after)
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if len(h.Runner.Reqs) != 1 {
				t.Fatalf("claude runs %d", len(h.Runner.Reqs))
			}
		})
	}
}

// A summary without a trunk-based block does not get one from a trivial
// increment (only an existing block is refreshed).
func TestTrivialIncrementAddsNoTBDBlock(t *testing.T) {
	h := trivialWS(t)
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if body := h.GL.Notes[500]; strings.Contains(body, marker.TBDStart) || !strings.Contains(body, summary.TrivialStatusPrefix) {
		t.Fatalf("summary:\n%s", body)
	}
}

// The stored summary note is gone, another one is found by its marker; its
// update fails: the state points at the note found, not the stale one.
func TestTrivialIncrementSummaryFallbackID(t *testing.T) {
	h := trivialWS(t)
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: trivOld, SummaryNoteID: 400}
	h.GL.Discussions[0].Notes[0].Body = strings.Replace(h.GL.Discussions[0].Notes[0].Body, marker.Summary(trivOld), marker.Summary("abc123"), 1)
	gl, _, _ := reviewtest.WithFaults(h)
	gl.Fail = reviewtest.FailOn(errBoom, "UpdateNote")
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.St.Get(); st.SummaryNoteID != 500 || st.ReviewedSHA != trivHead {
		t.Fatalf("state %+v", st)
	}
}

// The people to notify about what a trivial run could not check are marked
// notified once the summary went out.
func TestTrivialIncrementProblemsNotified(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "", "alice")
	h := ph.Harness
	setupTrivial(h, trivWSDiff, []string{"cache.go"}, trivWSBefore, trivWSAfter)
	h.Cfg.Review.Lint.Enabled = true
	h.Deps.Lint = &reviewtest.FakeLint{Res: &lint.Result{Tools: []lint.ToolStatus{{Tool: lint.ToolGitleaks, State: lint.StateFailed, Reason: "x"}}}}
	if err := ph.Svc.Handler(h.R().Handle)(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(ph.St.Rows) == 0 {
		t.Fatal("no problem recorded")
	}
	for _, r := range ph.St.Rows {
		if !r.NotifiedAt.Equal(reviewtest.TestNow) {
			t.Fatalf("not notified: %+v", r)
		}
	}
	if !strings.Contains(h.GL.Notes[500], "cc @alice") {
		t.Fatalf("summary:\n%s", h.GL.Notes[500])
	}
}
