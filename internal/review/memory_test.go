package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestMemoryDisabled(t *testing.T) {
	t.Run("repo", func(t *testing.T) {
		h := reviewtest.NewMemHarness(t)
		h.GL.RepoConfig = "memory:\n  enabled: false\n"
		h.Seed(t, memory.SectionTest, "- `make test-unit`\n")
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutMem(t, []memory.Update{{Section: "lint", Op: "add", Text: "make lint"}})}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		req := h.Runner.Reqs[0]
		if strings.Contains(req.Prompt, "project_memory") || strings.Contains(req.JSONSchema, "memory_updates") || strings.Contains(req.SystemPrompt, "Project memory") {
			t.Fatal("memory must be off for the repo")
		}
		if h.Section(t, memory.SectionLint) != nil {
			t.Fatal("updates must not be applied")
		}
	})
	t.Run("service", func(t *testing.T) {
		h := reviewtest.NewMemHarness(t)
		f := false
		h.Cfg.Review.Memory.Enabled = &f
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutMem(t, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.Runner.Reqs[0].Prompt, "project_memory") {
			t.Fatal("memory must be off for the service")
		}
	})
	t.Run("no store", func(t *testing.T) {
		h := reviewtest.New(t)
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutMem(t, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.Runner.Reqs[0].Prompt, "project_memory") || h.Runner.Reqs[0].JSONSchema != review.WithProblemsSchema(compatcheck.WithSchema(jsonschema.Review)) {
			t.Fatal("memory must be off without a store")
		}
	})
}
