package review_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/jira"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
)

func TestNoKeyAnywhere(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Title = "feat: добавить кэш"
	ft := &reviewtest.FakeTasks{}
	reviewtest.WithJira(h, ft)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(ft.Calls) != 0 {
		t.Fatalf("jira must not be called: %v", ft.Calls)
	}
	req := h.Runner.Reqs[0]
	if strings.Contains(req.Prompt, "untrusted_jira_issues") || req.JSONSchema != review.WithProblemsSchema(compatcheck.WithSchema(jsonschema.Review)) {
		t.Fatalf("no task check expected")
	}
	sum := reviewtest.SummaryBody(t, h)
	if strings.Contains(sum, task.SectionTitle) {
		t.Fatalf("no task section expected: %s", sum)
	}
	hb := reviewtest.HintBlock(sum)
	if !strings.Contains(hb, "не найден ключ задачи") || !strings.Contains(hb, "`feat: добавить кэш (PROJ-?)`") {
		t.Fatalf("hint: %s", hb)
	}
}

func TestJiraNotFoundContinues(t *testing.T) {
	h := reviewtest.New(t)
	ft := &reviewtest.FakeTasks{Errs: map[string]error{"PROJ-1": fmt.Errorf("x: %w", jira.ErrNotFound)}}
	reviewtest.WithJira(h, ft)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{{Path: "cache.go", Line: 2, Severity: "major", Title: "bug", Body: "b"}}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(h.GL.Published) != 1 {
		t.Fatalf("review must continue")
	}
	if h.Runner.Reqs[0].JSONSchema != review.WithProblemsSchema(compatcheck.WithSchema(jsonschema.Review)) {
		t.Fatalf("task_alignment must not be asked when no issue was fetched")
	}
	sum := reviewtest.SummaryBody(t, h)
	for _, s := range []string{task.SectionTitle, "[PROJ-1](https://jira.example.com/browse/PROJ-1)", "не удалось получить из Jira: задача не найдена", "Проверка соответствия задаче не выполнена"} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}

	h2 := reviewtest.New(t)
	reviewtest.WithJira(h2, &reviewtest.FakeTasks{Errs: map[string]error{"PROJ-1": jira.ErrForbidden}})
	h2.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if sum := reviewtest.SummaryBody(t, h2); !strings.Contains(sum, "нет доступа") {
		t.Fatalf("summary: %s", sum)
	}
}
