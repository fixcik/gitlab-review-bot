package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestFollowUpAddsFindingsInFlow(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Review.FollowUp = true
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOut(t, []output.Finding{
			{Path: "cache.go", Line: 2, Severity: "minor", Title: "Race on map", Body: "The map is accessed without a lock."},
		}, nil)},
		{Out: reviewtest.ReviewOut(t, []output.Finding{
			{Path: "cache.go", Line: 2, Severity: "minor", Title: "Race on map", Body: "The map is accessed without a lock at all."},
			{Path: "cache.go", Line: 4, Severity: "minor", Title: "Missing timeout", Body: "The call has no deadline."},
		}, nil)},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("want two Claude runs (review and second look), got %d", len(h.Runner.Reqs))
	}
	if !strings.Contains(h.Runner.Reqs[1].Prompt, "## Second review") || !strings.Contains(h.Runner.Reqs[1].Prompt, "Race on map") {
		t.Errorf("the second look lacks the first findings:\n%s", h.Runner.Reqs[1].Prompt)
	}
	if strings.Contains(h.Runner.Reqs[0].Prompt, "## Second review") {
		t.Errorf("the first run must not carry the second look block")
	}
	if len(h.GL.Published) != 2 {
		t.Fatalf("published %d notes, want 2 (the repeat dropped): %+v", len(h.GL.Published), h.GL.Published)
	}
}

func TestFollowUpOffInHarness(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("second look is off in the harness, got %d runs", len(h.Runner.Reqs))
	}
}

func TestRecallRuleEndsUserPrompt(t *testing.T) {
	for _, on := range []bool{false, true} {
		h := reviewtest.New(t)
		h.Cfg.Review.Recall = on
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if got := strings.HasSuffix(strings.TrimSpace(h.Runner.Reqs[0].Prompt), "Still report nothing you cannot tie to specific lines."); got != on {
			t.Errorf("recall=%v: coverage rule ends the user prompt = %v", on, got)
		}
	}
}
