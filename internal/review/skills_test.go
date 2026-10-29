package review_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func skillsHarness(t *testing.T) *reviewtest.Harness {
	t.Helper()
	h := reviewtest.New(t)
	dir := t.TempDir()
	for name, desc := range map[string]string{"go-style": "Стиль Go и ошибки", "migrations": "Миграции Postgres"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		md := "---\nname: " + name + "\ndescription: " + desc + "\n---\nПравила " + name
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.Cfg.Review.Skills.Dir = dir
	h.Deps.ScratchDir = t.TempDir()
	return h
}

func TestReviewOffersSkills(t *testing.T) {
	h := skillsHarness(t)
	var staged string
	h.Runner.OnRun = func(req core.RunRequest) {
		b, err := os.ReadFile(filepath.Join(req.SkillsDir, "go-style", "SKILL.md"))
		if err != nil {
			t.Errorf("staged skill not readable during the run: %v", err)
		}
		staged = req.SkillsDir
		if !strings.Contains(string(b), "Правила go-style") {
			t.Errorf("staged content %q", b)
		}
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ReviewOutput{Summary: "ok"}),
		Reads: []string{"/work/cache.go", "/skills/go-style/SKILL.md"}}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if req.SkillsDir == "" {
		t.Fatal("no skills dir")
	}
	for _, want := range []string{"## Скиллы команды", "- `go-style` — Стиль Go и ошибки (/skills/go-style/SKILL.md)",
		"- `migrations` — Миграции Postgres (/skills/migrations/SKILL.md)"} {
		if !strings.Contains(req.SystemPrompt, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staged copy not removed: %v", err)
	}
	sum := h.GL.Notes[h.GL.CreatedNotes[0]]
	if strings.Contains(sum, "📚") || strings.Contains(sum, "go-style") {
		t.Errorf("the summary must not list skills:\n%s", sum)
	}
}

func TestSkillsRepoDisableAndOff(t *testing.T) {
	h := skillsHarness(t)
	h.GL.RepoConfig = "skills:\n  disable: [Migrations]\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ReviewOutput{Summary: "ok"})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sys := h.Runner.Reqs[0].SystemPrompt
	if !strings.Contains(sys, "`go-style`") || strings.Contains(sys, "`migrations`") {
		t.Errorf("disable ignored:\n%s", sys)
	}
	if strings.Contains(h.GL.Notes[h.GL.CreatedNotes[0]], "📚") {
		t.Error("no skill was read, no summary line expected")
	}

	// Repo switch off, and service without a dir: no skills at all.
	for name, setup := range map[string]func(h *reviewtest.Harness){
		"repo off":   func(h *reviewtest.Harness) { h.GL.RepoConfig = "skills:\n  enabled: false\n" },
		"no service": func(h *reviewtest.Harness) { h.Cfg.Review.Skills = config.Skills{} },
	} {
		t.Run(name, func(t *testing.T) {
			h := skillsHarness(t)
			setup(h)
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ReviewOutput{Summary: "ok"})}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if req := h.Runner.Reqs[0]; req.SkillsDir != "" || strings.Contains(req.SystemPrompt, "Скиллы команды") {
				t.Errorf("skills offered: %q", req.SkillsDir)
			}
		})
	}
}

func TestChatSkillsLocalMode(t *testing.T) {
	h := skillsHarness(t)
	h.Cfg.Sandbox.Mode = "local"
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)}
	var root string
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Так принято."})}}
	// The read path is only known once the run starts (local mode: host path).
	h.Runner.OnRun = func(req core.RunRequest) {
		root = req.SkillsDir
		h.Runner.Resps[0].Reads = []string{filepath.Join(root, "migrations", "SKILL.md")}
	}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("почему так?")); err != nil {
		t.Fatal(err)
	}
	if sys := h.Runner.Reqs[0].SystemPrompt; !strings.Contains(sys, "("+filepath.Join(root, "go-style", "SKILL.md")+")") {
		t.Errorf("local mode must list host paths:\n%s", sys)
	}
	if r := reviewtest.LastReply(t, h.GL); strings.Contains(r.Body, "📚") || strings.Contains(r.Body, "migrations") {
		t.Errorf("the reply must not list skills: %s", r.Body)
	}
}

func TestRunsGetModelsAndHelpers(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ReviewOutput{Summary: "ok"})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if req.Model != "sonnet" || req.Advisor != "opus" || req.SearchModel != "haiku" || req.DeepReviewModel != "opus" {
		t.Fatalf("models %q %q %q %q", req.Model, req.Advisor, req.SearchModel, req.DeepReviewModel)
	}
	// The review run must call deep-review on a non-trivial MR; the advisor
	// stays as a one-line fallback.
	for _, s := range []string{"`code-search` subagent", "you MUST call the `deep-review` subagent with the Task tool exactly once before your final answer",
		"If it is unavailable or fails, finish without it.", "The advisor tool is a fallback"} {
		if !strings.Contains(req.SystemPrompt, s) {
			t.Errorf("helpers note lacks %q:\n%s", s, req.SystemPrompt)
		}
	}
	if strings.Contains(req.SystemPrompt, "consult the advisor tool once before you finish") {
		t.Error("the advisor must not be the main second opinion when deep-review is on")
	}

	h = reviewtest.New(t)
	h.Cfg.Claude.AdvisorModel, h.Cfg.Claude.SearchModel, h.Cfg.Claude.DeepReviewModel = "", "", ""
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ReviewOutput{Summary: "ok"})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if req := h.Runner.Reqs[0]; req.Advisor != "" || req.SearchModel != "" || req.DeepReviewModel != "" || strings.Contains(req.SystemPrompt, "## Helpers") {
		t.Errorf("helpers must be off: %+v", req.SystemPrompt)
	}
}
