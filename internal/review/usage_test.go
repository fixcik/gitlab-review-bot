package review_test

import (
	"context"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestUsageAutoReviewChargedToMRAuthor(t *testing.T) {
	h := reviewtest.New(t)
	l := reviewtest.Metered(h)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(l.Runs) != 1 {
		t.Fatalf("runs: %+v", l.Runs)
	}
	r := l.Runs[0]
	if r.Username != "alice" || r.Kind != meter.KindReview || r.ProjectPath != "group/api" || r.ProjectID != reviewtest.PID ||
		r.MRIID != reviewtest.IID || r.MRTitle != h.GL.MR.Title || !r.OK {
		t.Fatalf("attribution: %+v", r)
	}
}

func TestUsageManualCommandChargedToCaller(t *testing.T) {
	for _, kind := range []core.JobKind{core.JobReview, core.JobFullReview, core.JobChat} {
		h := reviewtest.New(t)
		l := reviewtest.Metered(h)
		out := reviewtest.ReviewOut(t, nil, nil)
		want := meter.KindReview
		if kind == core.JobChat {
			out, want = `{"reply":"ok","resolve":false}`, meter.KindChat
		}
		h.Runner.Resps = []reviewtest.RunResp{{Out: out}}
		if err := h.R().Handle(context.Background(), reviewtest.ManualJob(kind)); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if len(l.Runs) != 1 {
			t.Fatalf("%s runs: %+v", kind, l.Runs)
		}
		r := l.Runs[0]
		if r.Username != "bob" || r.UserID != reviewtest.UserID || r.Kind != want || r.MRIID != reviewtest.IID {
			t.Fatalf("%s attribution: %+v", kind, r)
		}
	}
}
