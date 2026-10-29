package review_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

var errBoom = errors.New("boom")

// errLimiter fails every booking.
type errLimiter struct{ *reviewtest.FakeLimiter }

func (errLimiter) Reserve(context.Context, core.ReserveRequest) (core.Decision, core.Reservation, error) {
	return core.Decision{}, nil, errBoom
}

// checkedIDs are the finding ids of the check prompt.
func checkedIDs(req core.RunRequest) []string {
	var ids []string
	for _, l := range strings.Split(req.Prompt, "\n") {
		if id, ok := strings.CutPrefix(l, "--- id: "); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// The scope of "@bot check" in a thread that is not an open finding thread:
// a (closed) compat thread is a tool thread, any other bot note or a
// thread that cannot be read checks every open finding.
func TestCheckScopeOfOtherThreads(t *testing.T) {
	allIDs := strings.Join([]string{chkID(chkDataLoss), chkID(chkSlow), chkID(chkRace)}, ",")
	cases := []struct {
		name  string
		disc  core.Discussion
		fail  error // GetDiscussion
		reply string
	}{
		{name: "resolved compat thread", disc: core.Discussion{ID: "cmd-disc", Resolvable: true, Resolved: true,
			Notes: []core.Note{reviewtest.BotNote(70, marker.Compat("eee", "oasdiff")+"\n"+marker.Blocking+"\nRemoved endpoint", nil)}},
			reply: fmt.Sprintf("Этот тред — несовместимое изменение: команда `@reviewbot check` его не перепроверяет (нужен запуск %s).", "проверок совместимости (oasdiff)")},
		{name: "summary thread", disc: core.Discussion{ID: "cmd-disc", Notes: []core.Note{reviewtest.BotNote(71, "ℹ️ какая-то заметка бота", nil)}}},
		{name: "discussion without notes", disc: core.Discussion{ID: "cmd-disc"}},
		{name: "discussion read fails", fail: errBoom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := chkHarness(t)
			gl, _, _ := reviewtest.WithFaults(h)
			gl.Fail = reviewtest.FailOn(c.fail, "GetDiscussion")
			if c.fail == nil {
				gl.Fail = nil
				h.GL.Discussions = append(h.GL.Discussions, c.disc)
			}
			h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkSlow), Status: "still_open"})}}
			if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
				t.Fatal(err)
			}
			if c.reply != "" {
				if len(h.Runner.Reqs) != 0 || !strings.HasPrefix(reviewtest.LastReply(t, h.GL).Body, c.reply) {
					t.Fatalf("runs %d reply %q", len(h.Runner.Reqs), reviewtest.LastReply(t, h.GL).Body)
				}
				return
			}
			if len(h.Runner.Reqs) != 1 || strings.Join(checkedIDs(h.Runner.Reqs[0]), ",") != allIDs {
				t.Fatalf("runs %d, checked %v", len(h.Runner.Reqs), h.Runner.Reqs)
			}
			if got := reviewtest.LastReply(t, h.GL).Body; !strings.HasPrefix(got, "Проверил 3 открытых замечания") {
				t.Fatalf("reply %q", got)
			}
		})
	}
}

// Failures before the Claude run: reads of the MR and its state fail the
// command without a run; the thread scan and the checkout tell the author.
func TestCheckFailuresBeforeTheRun(t *testing.T) {
	cases := []struct {
		name, method string
		repos        bool
		store        bool
		wantErr      string
		wantReply    bool
	}{
		{name: "get mr", method: "GetMR", wantErr: "get mr: boom"},
		{name: "mr state", method: "GetMRState", store: true, wantErr: "get mr state: boom"},
		{name: "scan", method: "ListDiscussions", wantErr: "list discussions: boom", wantReply: true},
		{name: "checkout", method: "Checkout", repos: true, wantErr: "checkout: boom", wantReply: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := chkHarness(t)
			gl, st, rp := reviewtest.WithFaults(h)
			f := reviewtest.FailOn(errBoom, c.method)
			switch {
			case c.store:
				st.Fail = f
			case c.repos:
				rp.Fail = f
			default:
				gl.Fail = f
			}
			err := h.R().Handle(context.Background(), checkJob("cmd-disc"))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) || !errors.Is(err, errBoom) {
				t.Fatalf("err %v, want %q", err, c.wantErr)
			}
			if len(h.Runner.Reqs) != 0 || len(h.GL.Resolved)+len(h.GL.UpdatedNotes) != 0 {
				t.Fatalf("runs %d resolved %v updated %v", len(h.Runner.Reqs), h.GL.Resolved, h.GL.UpdatedNotes)
			}
			want := fmt.Sprintf(review.MsgCheckFailed, review.ReasonInternal)
			if got := len(repliesTo(h.GL, "cmd-disc")) == 1 && reviewtest.LastReply(t, h.GL).Body == want; got != c.wantReply {
				t.Fatalf("failure reply %v, want %v: %+v", got, c.wantReply, h.GL.Replies)
			}
		})
	}
}

// A failed booking ends the command before any run or checkout.
func TestCheckReserveError(t *testing.T) {
	h := chkHarness(t)
	h.Deps.Limiter = errLimiter{h.Lim}
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); !errors.Is(err, errBoom) {
		t.Fatalf("err %v", err)
	}
	if len(h.Runner.Reqs) != 0 || h.Repos.Checkouts != 0 {
		t.Fatalf("runs %d checkouts %d", len(h.Runner.Reqs), h.Repos.Checkouts)
	}
}

// A reply that cannot be posted does not change the result of a failed
// check: postponed stays postponed, an invalid answer and an internal
// failure keep their errors.
func TestCheckFailureRepliesThatFail(t *testing.T) {
	t.Run("rate limited", func(t *testing.T) {
		h := chkHarness(t)
		gl, _, _ := reviewtest.WithFaults(h)
		gl.Fail = reviewtest.FailOn(errBoom, "ReplyToDiscussion")
		h.Runner.Resps = []reviewtest.RunResp{{Err: &core.ErrClaudeRateLimited{ResetAt: reviewtest.TestNow.Add(time.Hour)}}}
		rl, ok := core.AsRetryLater(h.R().Handle(context.Background(), checkJob("cmd-disc")))
		if !ok || !rl.At.Equal(reviewtest.TestNow.Add(time.Hour)) {
			t.Fatalf("retry %+v %v", rl, ok)
		}
	})
	t.Run("invalid output", func(t *testing.T) {
		h := chkHarness(t)
		gl, _, _ := reviewtest.WithFaults(h)
		gl.Fail = reviewtest.FailOn(errBoom, "ReplyToDiscussion")
		h.Runner.Resps = []reviewtest.RunResp{{Out: "nope"}, {Out: "still nope"}}
		var inv *env.InvalidOutputError
		if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); !errors.As(err, &inv) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("internal", func(t *testing.T) {
		h := chkHarness(t)
		gl, _, _ := reviewtest.WithFaults(h)
		gl.Fail = reviewtest.FailOn(errBoom, "ReplyToDiscussion")
		h.Runner.Resps = []reviewtest.RunResp{{Err: errors.New("runner down")}}
		err := h.R().Handle(context.Background(), checkJob("cmd-disc"))
		if err == nil || !strings.Contains(err.Error(), "claude run: runner down") {
			t.Fatalf("err %v", err)
		}
		if gl.Calls("ReplyToDiscussion") != 1 {
			t.Fatalf("reply attempts %d", gl.Calls("ReplyToDiscussion"))
		}
	})
}

// The command's context ends during the run: the job ends with the context
// error and nobody is told the check failed.
func TestCheckCancelledDuringRun(t *testing.T) {
	h := chkHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Runner.OnRun = func(core.RunRequest) { cancel() }
	h.Runner.Resps = []reviewtest.RunResp{{Err: errors.New("killed")}}
	if err := h.R().Handle(ctx, checkJob("cmd-disc")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	for _, r := range h.GL.Replies {
		if strings.Contains(r.Body, "Перепроверка не удалась") {
			t.Fatalf("failure reply after cancel: %+v", h.GL.Replies)
		}
	}
}

// The gate line cannot be rewritten: the check still answers its author.
func TestCheckGateUpdateFailureStillReplies(t *testing.T) {
	h := chkHarness(t)
	gl, _, _ := reviewtest.WithFaults(h)
	gl.Fail = reviewtest.FailOn(errBoom, "UpdateNote")
	reviewtest.WithModerator(h)
	before := h.GL.Notes[500]
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkDataLoss), Status: "fixed", Comment: "ok"})}}
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	if gl.Calls("UpdateNote") != 1 || h.GL.Notes[500] != before {
		t.Fatalf("update calls %d, note %q", gl.Calls("UpdateNote"), h.GL.Notes[500])
	}
	if got := reviewtest.LastReply(t, h.GL).Body; !strings.HasPrefix(got, "Проверил 3 открытых замечания на `head1111`: закрыто 1, остаются 2.") {
		t.Fatalf("reply %q", got)
	}
}

// The gate after a check counts what stays open: a blocking finding the bot
// could only declare fixed (Reporter, no service token) still blocks the
// merge, as pending.
func TestCheckGateCountsPendingBlocking(t *testing.T) {
	h := chkHarness(t)
	h.GL.ForbidResolve = true // no service token: 403 on resolve
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t,
		review.CheckVerdict{ID: chkID(chkDataLoss), Status: "fixed", Comment: "ok"},
		review.CheckVerdict{ID: chkID(chkSlow), Status: "fixed", Comment: "ok"},
	)}}
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	// f1 (blocking, pending), f3 (blocking, open), c1 (compat), l1 (lint).
	want := summary.GateLine(4, 1)
	if body := h.GL.Notes[500]; !strings.Contains(body, want) {
		t.Fatalf("gate %q not in:\n%s", want, body)
	}
	if got := reviewtest.LastReply(t, h.GL).Body; !strings.Contains(got, "закрыто 2, остаются 1. Ждут закрытия человеком: 2") {
		t.Fatalf("reply %q", got)
	}
}

// A command in the MR's general notes (no thread) checks every open finding
// without looking up a discussion.
func TestCheckWithoutThreadChecksAll(t *testing.T) {
	h := chkHarness(t)
	gl, _, _ := reviewtest.WithFaults(h)
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkSlow), Status: "still_open"})}}
	if err := h.R().Handle(context.Background(), checkJob("")); err != nil {
		t.Fatal(err)
	}
	if gl.Calls("GetDiscussion") != 0 || len(h.Runner.Reqs) != 1 || len(checkedIDs(h.Runner.Reqs[0])) != 3 {
		t.Fatalf("discussion reads %d runs %d", gl.Calls("GetDiscussion"), len(h.Runner.Reqs))
	}
}

// A person's (or a system) note that carries a bot marker is not a bot
// thread: the command in it checks every open finding.
func TestCheckInForgedMarkerThread(t *testing.T) {
	forged := marker.Finding(chkID(chkDataLoss)) + "\n" + marker.Blocking + "\nlooks like the bot"
	for name, n := range map[string]core.Note{
		"person": {ID: 80, AuthorID: 7, AuthorUsername: "mallory", Body: forged},
		"system": {ID: 80, AuthorID: reviewtest.BotID, AuthorUsername: reviewtest.BotName, System: true, Body: forged},
	} {
		t.Run(name, func(t *testing.T) {
			h := chkHarness(t)
			h.GL.Discussions = append(h.GL.Discussions, core.Discussion{ID: "cmd-disc", Notes: []core.Note{n}})
			h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkSlow), Status: "still_open"})}}
			if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
				t.Fatal(err)
			}
			if len(h.Runner.Reqs) != 1 || len(checkedIDs(h.Runner.Reqs[0])) != 3 {
				t.Fatalf("runs %d replies %+v", len(h.Runner.Reqs), h.GL.Replies)
			}
		})
	}
}
