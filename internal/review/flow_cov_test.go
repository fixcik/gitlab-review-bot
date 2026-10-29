package review_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

var msgFailedInternal = fmt.Sprintf("❌ Ревью не удалось: %s", review.ReasonInternal)

// replyBodies are the bodies of the replies to the command's thread.
func replyBodies(h *reviewtest.Harness) []string { return repliesTo(h.GL, "cmd-disc") }

// Failures of the reads before the Claude run: the review ends without a
// run and without moving the reviewed head; a command's author learns that
// it failed when the failure is past the preflight.
func TestReviewFailuresBeforeTheRun(t *testing.T) {
	cases := []struct {
		name, method string
		target       string // "gl", "store", "repos"
		wantErr      string
		wantReply    bool
	}{
		{name: "get mr", method: "GetMR", target: "gl", wantErr: "get mr: boom"},
		{name: "mr state", method: "GetMRState", target: "store", wantErr: "get mr state: boom"},
		{name: "checkout", method: "Checkout", target: "repos", wantErr: "checkout: boom", wantReply: true},
		{name: "changed files", method: "ChangedFiles", target: "repos", wantErr: "changed files: boom", wantReply: true},
		{name: "diff", method: "Diff", target: "repos", wantErr: "diff: boom", wantReply: true},
		{name: "thread scan", method: "ListDiscussions", target: "gl", wantErr: "list discussions: boom", wantReply: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := reviewtest.New(t)
			gl, st, rp := reviewtest.WithFaults(h)
			f := reviewtest.FailOn(errBoom, c.method)
			map[string]*reviewtest.Faults{"gl": &gl.Faults, "store": &st.Faults, "repos": &rp.Faults}[c.target].Fail = f
			err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) || !errors.Is(err, errBoom) {
				t.Fatalf("err %v, want %q", err, c.wantErr)
			}
			if len(h.Runner.Reqs) != 0 || len(h.Lim.Records()) != 0 {
				t.Fatalf("runs %d records %d", len(h.Runner.Reqs), len(h.Lim.Records()))
			}
			if st, ok := h.St.Get(); ok && st.ReviewedSHA != "" {
				t.Fatalf("state %+v", st)
			}
			got := replyBodies(h)
			if c.wantReply != (len(got) == 1 && got[0] == msgFailedInternal) {
				t.Fatalf("replies %q (want failure reply: %v)", got, c.wantReply)
			}
		})
	}
}

// An automatic review that fails is not answered (nobody asked).
func TestAutoReviewFailureNotAnswered(t *testing.T) {
	h := reviewtest.New(t)
	_, _, rp := reviewtest.WithFaults(h)
	rp.Fail = reviewtest.FailOn(errBoom, "Checkout")
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); !errors.Is(err, errBoom) {
		t.Fatalf("err %v", err)
	}
	if len(h.GL.Replies)+len(h.GL.CreatedNotes) != 0 {
		t.Fatalf("replies %+v notes %v", h.GL.Replies, h.GL.CreatedNotes)
	}
}

// The failure reply itself cannot be posted: the review's error stays.
func TestReviewFailureReplyFails(t *testing.T) {
	h := reviewtest.New(t)
	gl, _, rp := reviewtest.WithFaults(h)
	rp.Fail = reviewtest.FailOn(errBoom, "Checkout")
	gl.Fail = reviewtest.FailOn(errors.New("reply down"), "ReplyToDiscussion")
	err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview))
	if err == nil || err.Error() != "checkout: boom" {
		t.Fatalf("err %v", err)
	}
	if gl.Calls("ReplyToDiscussion") != 1 {
		t.Fatalf("reply attempts %d", gl.Calls("ReplyToDiscussion"))
	}
}

func TestReviewNotOpenedCommand(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.State = "merged"
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if got := replyBodies(h); len(got) != 1 || got[0] != review.MsgNotOpened || len(h.Runner.Reqs) != 0 {
		t.Fatalf("replies %q runs %d", got, len(h.Runner.Reqs))
	}
	// An automatic review of a closed MR ends silently.
	h2 := reviewtest.New(t)
	h2.GL.MR.State = "closed"
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h2.GL.Replies)+len(h2.GL.CreatedNotes)+len(h2.Runner.Reqs) != 0 {
		t.Fatalf("closed MR touched: %+v %v", h2.GL.Replies, h2.GL.CreatedNotes)
	}
}

// The ancestor check fails: the review falls back to the whole MR.
func TestReviewAncestorCheckErrorFullReview(t *testing.T) {
	h := reviewtest.New(t)
	_, _, rp := reviewtest.WithFaults(h)
	rp.Fail = reviewtest.FailOn(errBoom, "IsAncestor")
	h.St.States[core.MRKey(reviewtest.PID, reviewtest.IID)] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old000000000000"}
	h.Repos.Ancestors["old000000000000"] = true
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Repos.DiffFrom) == 0 || h.Repos.DiffFrom[0] != "base000000000000" || len(h.Runner.Reqs) != 1 {
		t.Fatalf("from %v runs %d", h.Repos.DiffFrom, len(h.Runner.Reqs))
	}
	if st, _ := h.St.Get(); st.ReviewedSHA != "head111111111111" {
		t.Fatalf("state %+v", st)
	}
}

// "Nothing to review" for a command whose reply cannot be posted: the head
// is recorded as reviewed anyway and the failed reply is only logged (the
// job does not fail, nothing runs the command again).
func TestNothingToReviewReplyFails(t *testing.T) {
	h := reviewtest.New(t)
	gl, _, _ := reviewtest.WithFaults(h)
	gl.Fail = reviewtest.FailOn(errBoom, "ReplyToDiscussion")
	h.Repos.Diff, h.Repos.Files = "", nil
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatalf("err %v", err)
	}
	if st, _ := h.St.Get(); st.ReviewedSHA != "head111111111111" || gl.Calls("ReplyToDiscussion") != 1 {
		t.Fatalf("state %+v replies %d", st, gl.Calls("ReplyToDiscussion"))
	}
	// The reply goes out: the head counts as reviewed.
	h2 := reviewtest.New(t)
	h2.Repos.Diff, h2.Repos.Files = "", nil
	if err := h2.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if st, _ := h2.St.Get(); st.ReviewedSHA != "head111111111111" {
		t.Fatalf("state %+v", st)
	}
	if got := replyBodies(h2); len(got) != 1 || !strings.Contains(got[0], "нет файлов для ревью") {
		t.Fatalf("replies %q", got)
	}
}

// A failed booking (limiter error) ends the review before Claude.
func TestReviewReserveError(t *testing.T) {
	h := reviewtest.New(t)
	h.Deps.Limiter = errLimiter{h.Lim}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); !errors.Is(err, errBoom) {
		t.Fatalf("err %v", err)
	}
	if len(h.Runner.Reqs) != 0 || len(h.GL.CreatedNotes) != 0 {
		t.Fatalf("runs %d notes %v", len(h.Runner.Reqs), h.GL.CreatedNotes)
	}
}

// cancelLint cancels the job while the analyzers run.
type cancelLint struct{ cancel context.CancelFunc }

func (c cancelLint) Check(context.Context, lint.Request) (*lint.Result, error) {
	c.cancel()
	return &lint.Result{}, nil
}

// The job is cancelled while the analyzers run: no Claude run, no summary,
// the context error.
func TestReviewCancelledDuringAnalyzers(t *testing.T) {
	h := reviewtest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Deps.Lint = cancelLint{cancel}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(ctx, reviewtest.AutoJob()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if len(h.Runner.Reqs) != 0 || len(h.GL.CreatedNotes) != 0 {
		t.Fatalf("runs %d notes %v", len(h.Runner.Reqs), h.GL.CreatedNotes)
	}
	if _, ok := h.St.Get(); ok {
		t.Fatal("state saved")
	}
}

// The job is cancelled during the Claude run: the context error, no failure
// note in the summary and no reply.
func TestReviewCancelledDuringRun(t *testing.T) {
	h := reviewtest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Runner.OnRun = func(core.RunRequest) { cancel() }
	h.Runner.Resps = []reviewtest.RunResp{{Err: errors.New("killed")}}
	if err := h.R().Handle(ctx, reviewtest.ManualJob(core.JobReview)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if len(h.GL.Replies)+len(h.GL.CreatedNotes) != 0 {
		t.Fatalf("replies %+v notes %v", h.GL.Replies, h.GL.CreatedNotes)
	}
}

// A command's replies after the run cannot be posted: the outcome of the
// review does not change (postponed, failed, done).
func TestReviewRepliesThatFail(t *testing.T) {
	newH := func(t *testing.T, resps ...reviewtest.RunResp) (*reviewtest.Harness, *reviewtest.FaultGitLab) {
		h := reviewtest.New(t)
		gl, _, _ := reviewtest.WithFaults(h)
		gl.Fail = reviewtest.FailOn(errors.New("reply down"), "ReplyToDiscussion")
		h.Runner.Resps = resps
		return h, gl
	}
	t.Run("postponed", func(t *testing.T) {
		h, gl := newH(t, reviewtest.RunResp{Err: &core.ErrClaudeRateLimited{ResetAt: reviewtest.TestNow.Add(time.Hour)}})
		rl, ok := core.AsRetryLater(h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)))
		if !ok || !rl.At.Equal(reviewtest.TestNow.Add(time.Hour)) || gl.Calls("ReplyToDiscussion") != 1 {
			t.Fatalf("retry %+v %v, replies %d", rl, ok, gl.Calls("ReplyToDiscussion"))
		}
	})
	t.Run("invalid output", func(t *testing.T) {
		h, gl := newH(t, reviewtest.RunResp{Out: "nope"}, reviewtest.RunResp{Out: "nope"})
		var inv *env.InvalidOutputError
		if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); !errors.As(err, &inv) || gl.Calls("ReplyToDiscussion") != 1 {
			t.Fatalf("err %v replies %d", err, gl.Calls("ReplyToDiscussion"))
		}
	})
	t.Run("done", func(t *testing.T) {
		h, gl := newH(t, reviewtest.RunResp{Out: reviewtest.ReviewOut(t, nil, nil)})
		if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
			t.Fatal(err)
		}
		if st, _ := h.St.Get(); st.ReviewedSHA != "head111111111111" || gl.Calls("ReplyToDiscussion") != 1 {
			t.Fatalf("state %+v replies %d", st, gl.Calls("ReplyToDiscussion"))
		}
	})
}

// The state cannot be saved after a review: the job fails (the queue retries
// it) and the command's author is not told it is done.
func TestReviewSaveStateError(t *testing.T) {
	h := reviewtest.New(t)
	_, st, _ := reviewtest.WithFaults(h)
	st.Fail = reviewtest.FailOn(errBoom, "SaveMRState")
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview))
	if err == nil || !strings.Contains(err.Error(), "save mr state: boom") {
		t.Fatalf("err %v", err)
	}
	if len(h.GL.CreatedNotes) == 0 {
		t.Fatal("summary not written before the state")
	}
	for _, r := range replyBodies(h) {
		if strings.HasPrefix(r, "✅ Ревью выполнено") {
			t.Fatalf("done reply although the state was not saved: %q", r)
		}
	}
}
