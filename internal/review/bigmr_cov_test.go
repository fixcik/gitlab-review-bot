package review_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// labelGitLab is a bot client that reads MR labels itself.
type labelGitLab struct {
	*reviewtest.FakeGitLab
	labels []string
	err    error
	reads  int
}

func (g *labelGitLab) MRLabels(context.Context, int64, int64) (string, []string, error) {
	g.reads++
	return "opened", g.labels, g.err
}

// The override label is read from the bot's client when it reads labels,
// else from the label client, else from the service token's; a label that
// cannot be read is no override.
func TestOverrideLabelSources(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(h *reviewtest.BigHarness) func() int // returns the reads
		reviewed bool
	}{
		{"bot client", func(h *reviewtest.BigHarness) func() int {
			gl := &labelGitLab{FakeGitLab: h.GL, labels: []string{"x", " BIG-MR-OK "}}
			h.Deps.GitLab = gl
			// The label client is not asked when the bot's client reads labels.
			h.Deps.Labels = &reviewtest.FakeLabelClient{Mr: &reviewtest.FakeMRLabels{State: "opened"}}
			return func() int { return gl.reads }
		}, true},
		{"service token only", func(h *reviewtest.BigHarness) func() int {
			lc := &reviewtest.FakeLabelClient{Mr: &reviewtest.FakeMRLabels{State: "opened", Labels: []string{"big-mr-ok"}}}
			h.Deps.ServiceLabels = lc
			return func() int { return lc.Reads }
		}, true},
		{"read error", func(h *reviewtest.BigHarness) func() int {
			gl := &labelGitLab{FakeGitLab: h.GL, labels: []string{"big-mr-ok"}, err: errBoom}
			h.Deps.GitLab = gl
			return func() int { return gl.reads }
		}, false},
		{"other labels", func(h *reviewtest.BigHarness) func() int {
			gl := &labelGitLab{FakeGitLab: h.GL, labels: []string{"big-mr", "ok"}}
			h.Deps.GitLab = gl
			return func() int { return gl.reads }
		}, false},
		{"no label reader", func(h *reviewtest.BigHarness) func() int { return func() int { return 1 } }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := reviewtest.NewBigHarness(t, 2100)
			reads := c.setup(h)
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if got := len(h.Runner.Reqs) == 1; got != c.reviewed || reads() == 0 {
				t.Fatalf("reviewed %v, want %v (reads %d)", got, c.reviewed, reads())
			}
			sum := h.Summary()
			if c.reviewed != strings.Contains(sum, "ревью выполнено по метке `big-mr-ok`") || c.reviewed == strings.Contains(sum, marker.TooBigStart) {
				t.Fatalf("summary:\n%s", sum)
			}
		})
	}
}

// The thread scan of a too-big MR fails: the job fails, a command's author
// is told, nothing is written.
func TestTooBigScanError(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	gl, _, _ := reviewtest.WithFaults(h.Harness)
	gl.Fail = reviewtest.FailOn(errBoom, "ListDiscussions")
	err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview))
	if !errors.Is(err, errBoom) {
		t.Fatalf("err %v", err)
	}
	if got := replyBodies(h.Harness); len(got) != 1 || got[0] != msgFailedInternal {
		t.Fatalf("replies %q", got)
	}
	if len(h.GL.CreatedNotes) != 0 || len(h.Fl.Calls) != 0 {
		t.Fatalf("notes %v lint %d", h.GL.CreatedNotes, len(h.Fl.Calls))
	}
}

// Open blocking finding threads of earlier reviews still block the merge
// of a too-big MR; non-blocking ones do not.
func TestTooBigGateCountsOpenFindings(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h.GL.Discussions = []core.Discussion{chkFindingDisc("f1", chkDataLoss, true), chkFindingDisc("f2", chkSlow, false),
		chkFindingDisc("f3", chkRace, true)}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if sum := h.Summary(); !strings.Contains(sum, summary.GateLine(2, 0)) {
		t.Fatalf("gate:\n%s", sum)
	}
}

// The summary of a too-big MR keeps the CI failure block of its head and
// lists what could not be checked.
func TestTooBigKeepsCIBlockAndProblems(t *testing.T) {
	const head = "abcdef0123456789"
	h := reviewtest.NewBigHarness(t, 2100)
	h.GL.MR.DiffRefs.HeadSHA = head
	h.Cfg.Review.Lint.Enabled = true
	h.Fl.Res = &lint.Result{Tools: []lint.ToolStatus{{Tool: lint.ToolGitleaks, State: lint.StateFailed, Reason: "панический выход"}}}
	ci := marker.CIBlockStart(core.Pipeline{ID: 77, SHA: head}, "") + "\n### 🔴 Пайплайн упал\n" + marker.CIBlockEnd
	h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(500,
		marker.Summary("0000000000000000")+"\n"+summary.Title+"\n\n"+summary.GateClearLine+"\n\n"+ci+"\n\nстарое\n", nil)}}}
	svc := problems.New(problems.Options{Store: &reviewtest.ProbStore{Rows: map[string]*problems.Record{}}, Now: func() time.Time { return reviewtest.TestNow }})
	if err := svc.Handler(h.R().Handle)(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sum := h.Summary()
	if !strings.Contains(sum, ci) || strings.Contains(sum, "старое") {
		t.Fatalf("CI block not kept:\n%s", sum)
	}
	if blk := reviewtest.ProblemsBlockOf(sum); !strings.Contains(blk, "gitleaks") {
		t.Fatalf("problems block %q in:\n%s", blk, sum)
	}
	// The CI block of another head is not kept.
	h2 := reviewtest.NewBigHarness(t, 2100)
	h2.GL.MR.DiffRefs.HeadSHA = "abcdef0000000000"
	h2.GL.Discussions = h.GL.Discussions[:1]
	h2.GL.Discussions[0].Notes[0].ID = 500
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if sum := h2.Summary(); strings.Contains(sum, "Пайплайн упал") {
		t.Fatalf("stale CI block kept:\n%s", sum)
	}
}

// Writes of a too-big review fail.
func TestTooBigWriteFailures(t *testing.T) {
	t.Run("summary", func(t *testing.T) {
		h := reviewtest.NewBigHarness(t, 2100)
		gl, _, _ := reviewtest.WithFaults(h.Harness)
		gl.Fail = reviewtest.FailOn(errBoom, "CreateNote")
		if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
			t.Fatal(err)
		}
		if _, ok := h.St.Get(); ok {
			t.Fatal("state saved without a summary note")
		}
		if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "MR слишком большой") {
			t.Fatalf("reply %+v", r)
		}
	})
	t.Run("state", func(t *testing.T) {
		h := reviewtest.NewBigHarness(t, 2100)
		_, st, _ := reviewtest.WithFaults(h.Harness)
		st.Fail = reviewtest.FailOn(errBoom, "SaveMRState")
		err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview))
		if err == nil || !strings.Contains(err.Error(), "save mr state: boom") {
			t.Fatalf("err %v", err)
		}
		if len(replyBodies(h.Harness)) != 0 {
			t.Fatalf("replied although the state was not saved: %q", replyBodies(h.Harness))
		}
	})
	t.Run("reply", func(t *testing.T) {
		h := reviewtest.NewBigHarness(t, 2100)
		gl, _, _ := reviewtest.WithFaults(h.Harness)
		gl.Fail = reviewtest.FailOn(errBoom, "ReplyToDiscussion")
		if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
			t.Fatal(err)
		}
		if st, ok := h.St.Get(); !ok || st.SummaryNoteID == 0 || gl.Calls("ReplyToDiscussion") != 1 {
			t.Fatalf("state %+v replies %d", st, gl.Calls("ReplyToDiscussion"))
		}
	})
	t.Run("size thread", func(t *testing.T) {
		// The blocking size thread cannot be posted: the merge stays
		// blocked (fail closed) and the summary says the thread is missing;
		// the next review posts it again.
		h := reviewtest.NewBigHarness(t, 2100)
		h.Cfg.Review.BigMR.Block = true
		h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
		h.GL.FailDraftFor = marker.TooBigThread
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		sum := h.Summary()
		if len(h.GL.Published) != 0 || !strings.Contains(sum, summary.GateLine(1, 0)) || !strings.Contains(sum, review.UnpublishedLine(1, "@reviewbot")) {
			t.Fatalf("published %+v\n%s", h.GL.Published, sum)
		}
		if st, _ := h.St.Get(); st.ReviewedSHA != "" {
			t.Fatalf("state %+v", st)
		}
	})
}

// The bot's threads cannot be read when it looks for its size thread: the
// open size thread of the scan keeps counting and no second one is posted.
func TestTooBigSizeThreadReadError(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Cfg.Review.BigMR.Block = true
	h.Fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.SecretFinding}, Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h.GL.Discussions = []core.Discussion{sizeThreadDisc("size", 600)}
	gl, _, _ := reviewtest.WithFaults(h.Harness)
	// Read 1: the config note; the scan comes from the job's memo; read 2
	// (after the secret thread was published): the size thread lookup.
	gl.Fail = reviewtest.FailNth("ListDiscussions", 7, errBoom)
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if gl.Calls("ListDiscussions") != 2 {
		t.Fatalf("reads %d", gl.Calls("ListDiscussions"))
	}
	for _, d := range h.GL.Published {
		if strings.Contains(d.Body, marker.TooBigThread) {
			t.Fatal("second size thread posted")
		}
	}
	if sum := h.Summary(); !strings.Contains(sum, summary.GateLine(2, 0)) { // the secret and the size thread
		t.Fatalf("gate:\n%s", sum)
	}
}

// After a review of a shrunk MR the size thread cannot be looked up: it
// keeps counting in the gate (still open in GitLab) and is not touched.
func TestClosedSizeThreadReadError(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 10)
	h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h.GL.Discussions = []core.Discussion{sizeThreadDisc("size", 600)}
	gl, _, _ := reviewtest.WithFaults(h.Harness)
	// The first read is the config note's; every later one fails: the size
	// thread's lookup after the review (and the bot's own resolving of the
	// non-blocking finding it published) cannot read the discussions.
	gl.Fail = func(m string, n int) error {
		if m == "ListDiscussions" && n >= 2 {
			return errBoom
		}
		return nil
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{chkSlow}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Resolved) != 0 || len(repliesTo(h.GL, "size")) != 0 {
		t.Fatalf("size thread touched: %v %+v", h.GL.Resolved, h.GL.Replies)
	}
	// Still open, not "fixed, waiting to be closed".
	if sum := h.Summary(); !strings.Contains(sum, summary.GateLine(1, 0)) {
		t.Fatalf("gate:\n%s", sum)
	}
}

// nothingToReview: the big change was reverted (empty range) on an MR with
// a summary (note 500, red gate) and an open size thread.
func nothingToReview(t *testing.T) *reviewtest.BigHarness {
	h := reviewtest.NewBigHarness(t, 10)
	h.Repos.Diff, h.Repos.Files = "", nil
	h.St.States[core.MRKey(reviewtest.PID, reviewtest.IID)] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old000000000000", SummaryNoteID: 500}
	h.Repos.Ancestors["old000000000000"] = true
	h.GL.Notes[500] = marker.Summary("old000000000000") + "\n" + summary.Title + "\n\n" + summary.GateLine(1, 0) + "\n\nтекст\n"
	h.GL.Discussions = []core.Discussion{
		{ID: "sum", Notes: []core.Note{reviewtest.BotNote(500, h.GL.Notes[500], nil)}},
		sizeThreadDisc("size", 600),
	}
	return h
}

// The size thread of a reverted big change cannot be resolved by the bot
// (Reporter, no service token): declared fixed, it still blocks the merge
// until a person closes it.
func TestNothingToReviewSizeThreadPending(t *testing.T) {
	h := nothingToReview(t)
	h.GL.ForbidResolve = true
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if r := repliesTo(h.GL, "size"); len(r) != 1 || !marker.HasFixed(r[0]) {
		t.Fatalf("replies %q", r)
	}
	if sum := h.GL.Notes[500]; !strings.Contains(sum, summary.GateLine(1, 1)) {
		t.Fatalf("gate:\n%s", sum)
	}
}

// The size thread of a reverted big change cannot be resolved (a GitLab
// error, not a missing permission): it still blocks, but it is not "fixed,
// waiting to be closed".
func TestNothingToReviewSizeThreadResolveFails(t *testing.T) {
	h := nothingToReview(t)
	h.GL.FailResolveFor = "size"
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if sum := h.GL.Notes[500]; !strings.Contains(sum, summary.GateLine(1, 0)) {
		t.Fatalf("gate:\n%s", sum)
	}
}

// After a review of a shrunk MR GitLab refuses to resolve the size thread:
// it stays open in the gate, not "fixed, waiting to be closed".
func TestClosedSizeThreadResolveFails(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 10)
	h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h.GL.Discussions = []core.Discussion{sizeThreadDisc("size", 600)}
	h.GL.FailResolveFor = "size"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if sum := h.Summary(); !strings.Contains(sum, summary.GateLine(1, 0)) {
		t.Fatalf("gate:\n%s", sum)
	}
}

// The threads cannot be read: the size thread and the gate stay as they
// are, the reviewed head still moves.
func TestNothingToReviewScanError(t *testing.T) {
	h := nothingToReview(t)
	gl, _, _ := reviewtest.WithFaults(h.Harness)
	gl.Fail = reviewtest.FailOn(errBoom, "ListDiscussions")
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Resolved)+len(h.GL.Replies)+len(h.GL.UpdatedNotes) != 0 {
		t.Fatalf("resolved %v replies %+v updated %v", h.GL.Resolved, h.GL.Replies, h.GL.UpdatedNotes)
	}
	if st, _ := h.St.Get(); st.ReviewedSHA != "head111111111111" {
		t.Fatalf("state %+v", st)
	}
}

// Without a size thread there is nothing to recount.
func TestNothingToReviewWithoutSizeThread(t *testing.T) {
	h := nothingToReview(t)
	h.GL.Discussions = h.GL.Discussions[:1]
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.UpdatedNotes) != 0 {
		t.Fatalf("summary rewritten: %v", h.GL.UpdatedNotes)
	}
}

func TestConfigCommandShowsNoSizeLimit(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Review.MaxMRLines = 0
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobConfig)); err != nil {
		t.Fatal(err)
	}
	if body := reviewtest.LastReply(t, h.GL).Body; !strings.Contains(body, "| порог размера MR (`max_mr_lines`) | без лимита |") {
		t.Fatalf("config:\n%s", body)
	}
}
