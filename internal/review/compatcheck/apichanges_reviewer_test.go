package compatcheck_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

func TestAPIChangesInPromptAndSummary(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Claude.SearchModel = "haiku"
	h.Compat.Res = &compat.Result{
		Changes: []compat.Change{reviewtest.OasdiffChange},
		API:     []compat.APIChanges{reviewtest.ApiOrders},
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if h.Compat.Calls[0].Lang != "ru" || h.Compat.Calls[0].SummaryOnly {
		t.Errorf("request: %+v", h.Compat.Calls[0])
	}
	p := h.Runner.Reqs[0].Prompt
	for _, s := range []string{
		"## API changes", "<untrusted_api_changes>", "spec: api/openapi.yaml",
		"Added endpoints:\n- ➕ GET /orders/{id}/history (line 35)",
		"Removed endpoints:\n- ⛔ DELETE /orders/{id}: API удалён без deprecation",
		"POST /orders:\n- ⛔ поле запроса `currency` стало обязательным (line 14)",
		"- ⚠️ добавлено значение enum `x` (api/schemas/order.yaml:7)",
		"- ➕ добавлено новое необязательное поле запроса `comment` (line 18)",
		"Outside endpoints (components, global settings):\n- ➕ схема `Order` добавлена",
		"already published as blocking threads",
		"handler / route", "consumers", "`code-search` subagent",
	} {
		if !strings.Contains(p, s) {
			t.Errorf("prompt lacks %q:\n%s", s, p)
		}
	}
	// The API section comes after the compat section.
	if strings.Index(p, "## Backward compatibility") > strings.Index(p, "## API changes") {
		t.Error("API changes must follow the compat section")
	}
	sum := reviewtest.SummaryBody(t, h)
	compatAt := strings.Index(sum, summary.CompatSectionTitle)
	apiAt := strings.Index(sum, "<details>\n<summary>📘 Изменения API: api/openapi.yaml — 3 ломающих, 1 потенциально ломающее, 4 совместимых; эндпоинты: +1, −1</summary>")
	textAt := strings.Index(sum, "Добавлен LRU-кэш.")
	if compatAt < 0 || apiAt < 0 || !(compatAt < apiAt && apiAt < textAt) {
		t.Fatalf("collapsed API block must follow the compat section:\n%s", sum)
	}
	for _, s := range []string{
		"**Новые эндпоинты:**\n- ➕ `GET /orders/{id}/history` · стр. 35",
		"**Удалённые эндпоинты:**\n- ⛔ `DELETE /orders/{id}` — API удалён без deprecation",
		"`POST /orders`\n- ⛔ поле запроса `currency` стало обязательным · стр. 14",
		"- ⚠️ добавлено значение enum `x` · `api/schemas/order.yaml:7`",
		"Вне эндпоинтов (компоненты, общие настройки):",
		"</details>",
	} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
	// Endpoint groups are sorted by path, then method.
	if strings.Index(sum, "`POST /orders`") > strings.Index(sum, "`GET /orders/{id}`") {
		t.Errorf("groups must be sorted by path:\n%s", sum)
	}
}

func TestAPIChangesWithoutSearchModel(t *testing.T) {
	got := compatcheck.RenderAPIPrompt(&compatcheck.APIRun{Specs: []compat.APIChanges{reviewtest.ApiOrders}}, true, false)
	if strings.Contains(got, "code-search") || !strings.Contains(got, "consumers") {
		t.Errorf("prompt:\n%s", got)
	}
	if compatcheck.RenderAPIPrompt(&compatcheck.APIRun{Specs: []compat.APIChanges{{Spec: "a.yaml"}}}, true, false) != "" || compatcheck.RenderAPIPrompt(nil, true, true) != "" {
		t.Error("no changes: no section")
	}
	if compatcheck.RenderAPISummary(&compatcheck.APIRun{Specs: []compat.APIChanges{{Spec: "a.yaml"}}}) != "" {
		t.Error("no changes: no block")
	}
}

func TestAPIChangesInCompatVerification(t *testing.T) {
	h := reviewtest.New(t)
	h.Compat.Res = &compat.Result{API: []compat.APIChanges{reviewtest.ApiOrders}}
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOutCompat(t, []output.BreakingChange{dropColumn}, nil)},
		{Out: verifyOut(t, output.CompatVerdict{Index: 1, Verdict: "confirmed", Reason: "да"})},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("runs: %d", len(h.Runner.Reqs))
	}
	v := h.Runner.Reqs[1].Prompt
	for _, s := range []string{"</untrusted_claims>", "not a claim to verify", "<untrusted_api_changes>", "POST /orders:"} {
		if !strings.Contains(v, s) {
			t.Errorf("verification prompt lacks %q:\n%s", s, v)
		}
	}
	if strings.Index(v, "</untrusted_claims>") > strings.Index(v, "<untrusted_api_changes>") {
		t.Error("API context must not be inside the claims")
	}
}
