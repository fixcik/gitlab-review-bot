package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

const pathsRepoConfig = `path_instructions:
  - path: "migrations/**"
    instructions: "Проверяй обратимость и блокировки таблиц"
  - path: "docs/**"
    instructions: "Проверяй орфографию в документации"
`

func TestReviewPromptOnlyMatchingPathInstructions(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Review.PathInstructions = []config.PathInstruction{{Path: "**/*.go", Instructions: "Сервис: проверяй закрытие ресурсов"}}
	h.GL.RepoConfig = pathsRepoConfig
	h.Repos.Files = []string{"cache.go", "migrations/0001_init.sql"}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	for _, s := range []string{
		"Path-specific review instructions", "<path_instructions>", "</path_instructions>",
		"glob: migrations/**", "files: migrations/0001_init.sql", "Проверяй обратимость и блокировки таблиц",
		"glob: **/*.go", "files: cache.go", "Сервис: проверяй закрытие ресурсов",
	} {
		if !strings.Contains(req.Prompt, s) {
			t.Errorf("prompt lacks %q:\n%s", s, req.Prompt)
		}
	}
	if strings.Contains(req.Prompt, "docs/**") || strings.Contains(req.Prompt, "орфографию") {
		t.Errorf("non-matching instruction leaked into the prompt:\n%s", req.Prompt)
	}
	// Service entries come first.
	if strings.Index(req.Prompt, "glob: **/*.go") > strings.Index(req.Prompt, "glob: migrations/**") {
		t.Error("service instructions must precede repo instructions")
	}
	if !strings.Contains(req.SystemPrompt, "Path-specific review instructions") || !strings.Contains(req.SystemPrompt, "never change the required JSON output") {
		t.Errorf("system prompt lacks the trust note:\n%s", req.SystemPrompt)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, "📁 Применены инструкции для путей: `**/*.go`, `migrations/**`") {
		t.Errorf("summary lacks the path line:\n%s", sum)
	}
	if !strings.Contains(sum, "<details>") || strings.Contains(sum, "docs/**") {
		t.Errorf("summary path block:\n%s", sum)
	}
}

func TestReviewNoPathInstructionsWhenNothingMatches(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.RepoConfig = pathsRepoConfig
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if strings.Contains(req.Prompt, "Path-specific") {
		t.Errorf("no instruction matches cache.go:\n%s", req.Prompt)
	}
	// The system note depends on the repository's configuration only (prompt
	// caching): the repository has path instructions, so it is there, worded
	// conditionally.
	if !strings.Contains(req.SystemPrompt, "When the prompt contains a \"Path-specific review instructions\" section") {
		t.Errorf("system note:\n%s", req.SystemPrompt)
	}
	if strings.Contains(reviewtest.SummaryBody(t, h), "📁") {
		t.Error("summary must not mention path instructions")
	}
}

func TestReviewBadPathGlobIsConfigError(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.RepoConfig = "path_instructions:\n  - path: \"migrations/[\"\n    instructions: x\n"
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	r := reviewtest.LastReply(t, h.GL)
	if !strings.Contains(r.Body, "Не удалось разобрать `.reviewbot.yaml`") || !strings.Contains(r.Body, "invalid glob") {
		t.Fatalf("reply %+v", r)
	}
	if len(h.Runner.Reqs) != 0 {
		t.Fatal("no run with a broken config")
	}
}

func TestChatIncludesThreadFilePathInstructions(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.RepoConfig = `path_instructions:
  - path: "*.go"
    instructions: "Go: проверяй гонки"
  - path: "docs/**"
    instructions: "Документация"
  - path: "api/**"
    instructions: "API: контракты"
`
	h.Repos.Files = []string{"api/v1.yaml"}
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)} // attached to cache.go
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Ок."})}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("а так?")); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	for _, s := range []string{"Path-specific review instructions", "glob: *.go", "files: cache.go", "Go: проверяй гонки", "glob: api/**", "API: контракты"} {
		if !strings.Contains(req.Prompt, s) {
			t.Errorf("chat prompt lacks %q:\n%s", s, req.Prompt)
		}
	}
	if strings.Contains(req.Prompt, "Документация") {
		t.Error("non-matching instruction in chat prompt")
	}
	if !strings.Contains(req.SystemPrompt, "Path-specific review instructions") {
		t.Error("chat system prompt lacks the trust note")
	}
}
