package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

func TestAPIChangesCompatDisabledSummaryOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *reviewtest.Harness)
	}{
		{"repo", func(h *reviewtest.Harness) {
			h.GL.RepoConfig = "compat:\n  enabled: false\n  openapi: [\"contracts/*.yml\"]\n"
		}},
		{"service", func(h *reviewtest.Harness) { h.Cfg.Review.CompatCheck = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := reviewtest.New(t)
			tc.set(h)
			// Even if the checker returned breaking changes, nothing may be
			// published with the check off.
			h.Compat.Res = &compat.Result{Changes: []compat.Change{reviewtest.OasdiffChange}, API: []compat.APIChanges{reviewtest.ApiOrders}}
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if len(h.Compat.Calls) != 1 || !h.Compat.Calls[0].SummaryOnly {
				t.Fatalf("calls: %+v", h.Compat.Calls)
			}
			if tc.name == "repo" && strings.Join(h.Compat.Calls[0].Config.OpenAPI, ",") != "contracts/*.yml" {
				t.Errorf("extra OpenAPI globs must still apply: %+v", h.Compat.Calls[0].Config)
			}
			req := h.Runner.Reqs[0]
			if req.JSONSchema != review.WithProblemsSchema(jsonschema.Review) || strings.Contains(req.Prompt, "## Backward compatibility") {
				t.Fatal("compat must not be asked for")
			}
			if !strings.Contains(req.Prompt, "## API changes") || !strings.Contains(req.Prompt, "turned off for this repository") {
				t.Errorf("prompt:\n%s", req.Prompt)
			}
			if n := len(reviewtest.CompatThreads(h)); n != 0 {
				t.Fatalf("no compat threads with the check off: %d", n)
			}
			sum := reviewtest.SummaryBody(t, h)
			if strings.Contains(sum, summary.CompatSectionTitle) || !strings.Contains(sum, "📘 Изменения API: api/openapi.yaml") ||
				!strings.Contains(sum, "✅ Блокирующих замечаний нет.") {
				t.Errorf("summary:\n%s", sum)
			}
		})
	}
}
