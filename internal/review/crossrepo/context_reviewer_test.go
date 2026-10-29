package crossrepo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func contextHarness(t *testing.T) (*reviewtest.Harness, *reviewtest.FakeContextRepos) {
	t.Helper()
	h := reviewtest.New(t)
	h.Cfg.GitLab.URL = "https://gitlab.example.com"
	h.Cfg.GitLab.Groups = []string{"group", "billing"}
	f := &reviewtest.FakeContextRepos{Projects: []string{"billing/api", "group/api", "group/web", "legacy/old"}}
	h.Deps.ContextRepos = f
	return h, f
}

func TestReviewPromptListsContextRepos(t *testing.T) {
	h, f := contextHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ReviewOutput{
		Summary: "Потребитель: billing/api:client.go:42 и `group/web:src/app.ts:7`.",
		Findings: []output.Finding{{Path: "cache.go", Line: 2, Severity: "major", Title: "Breaks consumer",
			Body: "Используется в billing/api:pkg/charge.go:10-12, см. также unknown/repo:x.go:1.\n```go\nbilling/api:pkg/charge.go:10\n```"}},
	})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	// The MR's own project (group/api) is excluded.
	var names []string
	for _, d := range req.ContextDirs {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "billing/api,group/web,legacy/old" {
		t.Fatalf("context dirs %v", names)
	}
	for _, want := range []string{
		"<context_repositories>", "</context_repositories>",
		"- billing/api → /repos/billing/api (main @ c0ffee00)",
		"- group/web → /repos/group/web",
		"consumers and producers", "UNTRUSTED DATA", "group/project:path:line",
	} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(req.Prompt, "group/api →") {
		t.Error("own project listed as context repo")
	}
	if f.Released != 1 {
		t.Errorf("release calls %d", f.Released)
	}
	// Citations are rendered as links in the published finding and summary.
	sha := "c0ffee" + strings.Repeat("0", 34)
	body := h.GL.Published[0].Body
	if !strings.Contains(body, "[billing/api:pkg/charge.go:10-12](https://gitlab.example.com/billing/api/-/blob/"+sha+"/pkg/charge.go#L10-12)") {
		t.Errorf("finding body not linked:\n%s", body)
	}
	if !strings.Contains(body, "unknown/repo:x.go:1.") || strings.Contains(body, "[unknown/repo") {
		t.Errorf("unknown repo must stay as is:\n%s", body)
	}
	if !strings.Contains(body, "```go\nbilling/api:pkg/charge.go:10\n```") {
		t.Errorf("code block changed:\n%s", body)
	}
	sum := h.GL.Notes[h.GL.CreatedNotes[0]]
	if !strings.Contains(sum, "[`group/web:src/app.ts:7`](https://gitlab.example.com/group/web/-/blob/"+sha+"/src/app.ts#L7)") {
		t.Errorf("summary not linked:\n%s", sum)
	}
}

func TestReviewContextExcludeAndDisable(t *testing.T) {
	h, _ := contextHarness(t)
	h.GL.RepoConfig = "context:\n  exclude: [\"legacy/**\", \"Group/Web\"]\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := h.Runner.Reqs[0].ContextDirs; len(got) != 1 || got[0].Name != "billing/api" {
		t.Fatalf("context dirs %+v", got)
	}

	h, f := contextHarness(t)
	h.GL.RepoConfig = "context:\n  enabled: false\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs[0].ContextDirs) != 0 || strings.Contains(h.Runner.Reqs[0].Prompt, "context_repositories") || f.Calls != 0 {
		t.Errorf("disabled repo got context: %+v", h.Runner.Reqs[0].ContextDirs)
	}
}

func TestReviewContextOffWithoutGroups(t *testing.T) {
	for _, mod := range []func(h *reviewtest.Harness){
		func(h *reviewtest.Harness) { h.Cfg.GitLab.Groups = nil },
		func(h *reviewtest.Harness) { h.Cfg.Review.Context.Enabled = false },
		func(h *reviewtest.Harness) { h.Deps.ContextRepos = nil },
	} {
		h, f := contextHarness(t)
		mod(h)
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		if len(h.Runner.Reqs[0].ContextDirs) != 0 || strings.Contains(h.Runner.Reqs[0].Prompt, "context_repositories") || f.Calls != 0 {
			t.Errorf("context must be off: %+v", h.Runner.Reqs[0].ContextDirs)
		}
	}
}

func TestChatContextReposAndLinks(t *testing.T) {
	h, f := contextHarness(t)
	h.Cfg.Sandbox.Mode = "local"
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Вызывается в billing/api:client.go:42."})}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("где используется?")); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if len(req.ContextDirs) != 3 {
		t.Fatalf("dirs %+v", req.ContextDirs)
	}
	// Local mode: host paths in the prompt.
	if !strings.Contains(req.Prompt, "- billing/api → /data/snapshots/billing/api (main @ c0ffee00)") {
		t.Errorf("prompt:\n%s", req.Prompt)
	}
	if !strings.Contains(req.Prompt, "how the code is used elsewhere") {
		t.Error("chat wording missing")
	}
	r := reviewtest.LastReply(t, h.GL)
	if !strings.Contains(r.Body, "[billing/api:client.go:42](https://gitlab.example.com/billing/api/-/blob/c0ffee") {
		t.Errorf("reply not linked: %s", r.Body)
	}
	if f.Released != 1 {
		t.Errorf("released %d", f.Released)
	}
}

func TestReviewRelatedRepos(t *testing.T) {
	h, f := contextHarness(t)
	h.GL.RepoConfig = `context:
  exclude: ["group/web", "legacy/**"]
  related:
    - repo: group/web
      note: "фронтенд, вызывает наш REST API из src/api"
    - repo: outside/mobile
    - repo: group/api
    - repo: group/typo
`
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ReviewOutput{Summary: "ok"})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	// The reviewed project is never asked for, even when listed as related.
	if strings.Join(f.Related, ",") != "group/web,outside/mobile,group/typo" {
		t.Fatalf("related asked %v", f.Related)
	}
	req := h.Runner.Reqs[0]
	var names []string
	for _, d := range req.ContextDirs {
		names = append(names, d.Name)
	}
	// Related first; group/web is excluded but related, legacy/old excluded.
	if strings.Join(names, ",") != "group/web,outside/mobile,billing/api" {
		t.Fatalf("dirs %v", names)
	}
	p := req.Prompt
	for _, want := range []string{
		"Related repositories — the team named them as working together with group/api",
		"- group/web → /repos/group/web (main @ c0ffee00) — фронтенд, вызывает наш REST API из src/api",
		"- outside/mobile → /repos/outside/mobile (main @ c0ffee00)\n",
		"Other repositories of the same GitLab groups:\n- billing/api → /repos/billing/api",
		"for example `group/web:path/to/file.go:42`",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q\n%s", want, p)
		}
	}
	if strings.Index(p, "group/web →") > strings.Index(p, "billing/api →") {
		t.Error("related must come before the others")
	}
	sum := h.GL.Notes[h.GL.CreatedNotes[0]]
	if !strings.Contains(sum, "⚠️ Связанные репозитории из `context.related` недоступны") ||
		!strings.Contains(sum, "- `group/typo` — не найден или у бота нет доступа на чтение") || strings.Contains(sum, "outside/mobile") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestContextPromptWithoutRelated(t *testing.T) {
	h, _ := contextHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ReviewOutput{Summary: "ok"})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	p := h.Runner.Reqs[0].Prompt
	if strings.Contains(p, "Related repositories") || !strings.Contains(p, "Read-only snapshots of 3 other repositories") {
		t.Errorf("prompt:\n%s", p)
	}
	if strings.Contains(h.GL.Notes[h.GL.CreatedNotes[0]], "context.related") {
		t.Error("no warning expected")
	}
}
