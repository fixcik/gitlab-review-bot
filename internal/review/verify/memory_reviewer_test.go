package verify_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

func TestMemoryInjectedIntoVerificationPrompt(t *testing.T) {
	h := reviewtest.NewMemHarness(t)
	h.Seed(t, memory.SectionStructure, "- Миграции: `db/migrations`, читает их `internal/auth`\n")
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOutMem(t, nil)},
		{Out: verifyOut(t, output.CompatVerdict{Index: 1, Verdict: "confirmed", Reason: "auth.go читает email"})},
	}
	h.Runner.Resps[0].Out = func() string {
		var m map[string]any
		json.Unmarshal([]byte(reviewtest.ReviewOutCompat(t, []output.BreakingChange{dropColumn}, nil)), &m)
		return reviewtest.MustJSON(t, m)
	}()
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("runs %d", len(h.Runner.Reqs))
	}
	v := h.Runner.Reqs[1]
	if !strings.Contains(v.Prompt, "<project_memory>") || !strings.Contains(v.Prompt, "db/migrations") || !strings.Contains(v.SystemPrompt, "Project memory") {
		t.Fatalf("verification prompt lacks memory:\n%s", v.Prompt)
	}
	if strings.Contains(v.Prompt, "memory_updates") || v.JSONSchema != verify.ClaimsSchema || v.NoTools {
		t.Fatalf("verification must stay read-only with its own schema")
	}
}
