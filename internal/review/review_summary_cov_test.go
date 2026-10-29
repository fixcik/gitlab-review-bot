package review_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// The summary note of a finished review cannot be written: the review still
// counts as done (its threads are out); the state keeps the note it knows
// when the update failed, none when the note could not be created.
func TestReviewSummaryWriteError(t *testing.T) {
	t.Run("update", func(t *testing.T) {
		h := reviewtest.New(t)
		h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, SummaryNoteID: 500}
		h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(500, marker.Summary("abc")+"\nold", nil)}}}
		gl, _, _ := reviewtest.WithFaults(h)
		gl.Fail = reviewtest.FailOn(errBoom, "UpdateNote")
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		if st, _ := h.St.Get(); st.ReviewedSHA != "head111111111111" || st.SummaryNoteID != 500 {
			t.Fatalf("state %+v", st)
		}
		if len(h.GL.CreatedNotes) != 0 {
			t.Fatalf("a second summary was created: %v", h.GL.CreatedNotes)
		}
	})
	t.Run("create", func(t *testing.T) {
		h := reviewtest.New(t)
		gl, _, _ := reviewtest.WithFaults(h)
		gl.Fail = reviewtest.FailOn(errBoom, "CreateNote")
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		if st, _ := h.St.Get(); st.ReviewedSHA != "head111111111111" || st.SummaryNoteID != 0 {
			t.Fatalf("state %+v", st)
		}
	})
}

// postponedAuto runs an automatic review the global limit postpones.
func postponedAuto(t *testing.T, h *reviewtest.Harness) {
	t.Helper()
	at := reviewtest.TestNow.Add(time.Hour)
	h.Lim.Global = core.Decision{RetryAt: at, Reason: "Глобальный лимит ревью исчерпан."}
	rl, ok := core.AsRetryLater(h.R().Handle(context.Background(), reviewtest.AutoJob()))
	if !ok || !rl.At.Equal(at) {
		t.Fatalf("retry %+v %v", rl, ok)
	}
}

// The remembered summary note is gone: the status goes into a fresh note
// and the state points at it, keeping the reviewed head.
func TestPostponedStatusSummaryGone(t *testing.T) {
	h := reviewtest.New(t)
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old", SummaryNoteID: 500}
	h.Repos.Ancestors["old"] = true
	// A Claude rate limit after the scan found no summary (threads.SummaryID 0).
	h.Runner.Resps = []reviewtest.RunResp{{Err: &core.ErrClaudeRateLimited{ResetAt: reviewtest.TestNow.Add(time.Hour)}}}
	if _, ok := core.AsRetryLater(h.R().Handle(context.Background(), reviewtest.AutoJob())); !ok {
		t.Fatal("want RetryLater")
	}
	if len(h.GL.CreatedNotes) != 1 || !strings.Contains(h.GL.Notes[h.GL.CreatedNotes[0]], "⏳ Ревью отложено до 13:00") {
		t.Fatalf("notes %v", h.GL.Notes)
	}
	if st, _ := h.St.Get(); st.SummaryNoteID != h.GL.CreatedNotes[0] || st.ReviewedSHA != "old" {
		t.Fatalf("state %+v", st)
	}
}

// Failures while writing the postponed status: the job is postponed all
// the same.
func TestPostponedStatusFailures(t *testing.T) {
	t.Run("threads unreadable", func(t *testing.T) {
		h := reviewtest.New(t)
		gl, _, _ := reviewtest.WithFaults(h)
		gl.Fail = reviewtest.FailOn(errBoom, "ListDiscussions")
		postponedAuto(t, h)
		if len(h.GL.CreatedNotes)+len(h.GL.UpdatedNotes) != 0 {
			t.Fatalf("status written without the threads: %v %v", h.GL.CreatedNotes, h.GL.UpdatedNotes)
		}
		if _, ok := h.St.Get(); ok {
			t.Fatal("state saved")
		}
	})
	t.Run("note not written", func(t *testing.T) {
		h := reviewtest.New(t)
		gl, _, _ := reviewtest.WithFaults(h)
		gl.Fail = reviewtest.FailOn(errBoom, "CreateNote")
		postponedAuto(t, h)
		if _, ok := h.St.Get(); ok {
			t.Fatal("state saved without a note")
		}
	})
	t.Run("state not saved", func(t *testing.T) {
		h := reviewtest.New(t)
		_, st, _ := reviewtest.WithFaults(h)
		st.Fail = reviewtest.FailOn(errBoom, "SaveMRState")
		postponedAuto(t, h)
		if len(h.GL.CreatedNotes) != 1 || st.Calls("SaveMRState") != 1 {
			t.Fatalf("notes %v saves %d", h.GL.CreatedNotes, st.Calls("SaveMRState"))
		}
	})
}
