package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

func TestCompatDisabledPerRepo(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.RepoConfig = "compat:\n  enabled: false\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	// Only the API changelog runs (no blocking checks).
	if len(h.Compat.Calls) != 1 || !h.Compat.Calls[0].SummaryOnly {
		t.Fatalf("only the summary-only check may run: %+v", h.Compat.Calls)
	}
	req := h.Runner.Reqs[0]
	if req.JSONSchema != review.WithProblemsSchema(jsonschema.Review) || strings.Contains(req.SystemPrompt, "Backward compatibility") || strings.Contains(req.Prompt, "## Backward compatibility") {
		t.Fatalf("compat must not be asked for")
	}
	if strings.Contains(reviewtest.SummaryBody(t, h), summary.CompatSectionTitle) {
		t.Fatal("no compat section expected")
	}
}

func TestCompatDisabledGlobally(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Review.CompatCheck = false
	h.GL.RepoConfig = "compat:\n  enabled: true\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Compat.Calls) != 1 || !h.Compat.Calls[0].SummaryOnly || h.Runner.Reqs[0].JSONSchema != review.WithProblemsSchema(jsonschema.Review) {
		t.Fatal("service switch must win over the repo (summary only)")
	}
}
