package pipeline_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestCoverageFromArtifact(t *testing.T) {
	h := reviewtest.CoverageHarness(t)
	var files []string
	h.Runner.OnRun = func(req core.RunRequest) {
		ents, _ := os.ReadDir(h.Repos.Dir)
		for _, e := range ents {
			files = append(files, e.Name())
		}
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	// Nothing is written into the clone: Claude has no tool to run on it;
	// the bot computed the uncovered changed lines (MCP off: in the prompt,
	// at its end; the system prompt has no per-MR facts: prompt caching).
	if len(files) != 0 {
		t.Errorf("clone files %v", files)
	}
	for _, want := range []string{
		"50.0% of the changed executable lines are covered (1 of 2)",
		"Uncovered changed lines (new line numbers), least covered first:\n- cache.go: 1 of 2 covered; uncovered changed lines 3\n",
		"report it as a finding on that code",
	} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt note lacks %q:\n%s", want, req.Prompt)
		}
	}
	if strings.Contains(req.SystemPrompt, "Test coverage") || strings.Contains(req.SystemPrompt, "cache.go") {
		t.Errorf("coverage facts in the system prompt:\n%s", req.SystemPrompt)
	}
	if strings.Contains(req.Prompt, "diffcov") || strings.Contains(req.Prompt, ".reviewbot-coverage") {
		t.Errorf("prompt still points to the diffcov tool:\n%s", req.Prompt)
	}
	sum := reviewtest.SummaryBody(t, h)
	blk := marker.CoverageRe.FindString(sum)
	for _, want := range []string{
		"<summary>🧪 Покрытие нового кода: 50% (1 из 2 строк) — ⚠️ ниже 80%</summary>",
		"| `cache.go` | 50% (1/2) | 3 |",
		"Нет в отчёте: `svc.go`",
		"Отчёт: [tests 1/2](https://gitlab.example.com/group/api/-/jobs/1), `tests 2/2` · пайплайн [#77](https://gitlab.example.com/group/api/-/pipelines/77) · cobertura",
	} {
		if !strings.Contains(blk, want) {
			t.Errorf("block lacks %q:\n%s", want, blk)
		}
	}
	if !(strings.Index(sum, marker.TBDEnd) < strings.Index(sum, marker.CoverageStart)) {
		t.Error("coverage goes after the TBD block")
	}
}

func TestCoverageStates(t *testing.T) {
	cases := map[string]struct {
		setup func(h *reviewtest.Harness)
		want  string
	}{
		"pipeline failed":  {func(h *reviewtest.Harness) { h.GL.MR.Pipeline.Status = "failed" }, "⚠️ пайплайн упал — отчёта о покрытии нет"},
		"no pipeline":      {func(h *reviewtest.Harness) { h.GL.MR.Pipeline = nil }, "у MR нет пайплайна"},
		"running":          {func(h *reviewtest.Harness) { h.GL.MR.Pipeline.Status = "running" }, "⏳ жду завершения пайплайна"},
		"older commit":     {func(h *reviewtest.Harness) { h.GL.MR.Pipeline.SHA = "old" }, "⏳ жду завершения пайплайна"},
		"job not found":    {func(h *reviewtest.Harness) { h.GL.RepoConfig = "coverage:\n  job: unit\n  path: c.xml\n" }, "джоба `unit` не найдена в пайплайне"},
		"artifact missing": {func(h *reviewtest.Harness) { h.GL.Artifacts = nil }, "в артефактах джобы `tests 1/2` нет файла `build/coverage.xml`"},
		"bad report": {func(h *reviewtest.Harness) {
			h.GL.Artifacts = map[string][]byte{"1:build/coverage.xml": []byte("junk")}
		}, "не удалось разобрать `build/coverage.xml`"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := reviewtest.CoverageHarness(t)
			tc.setup(h)
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if blk := marker.CoverageRe.FindString(reviewtest.SummaryBody(t, h)); !strings.Contains(blk, tc.want) {
				t.Errorf("block lacks %q:\n%s", tc.want, blk)
			}
			if strings.Contains(h.Runner.Reqs[0].SystemPrompt, "Test coverage of the changed lines") {
				t.Error("no report: no coverage section")
			}
		})
	}

	h := reviewtest.New(t) // not configured
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reviewtest.SummaryBody(t, h), "reviewbot:coverage") {
		t.Error("not configured: no block")
	}
}

func TestCoverageRefreshAfterPipeline(t *testing.T) {
	h := reviewtest.CoverageHarness(t)
	h.Cfg.Review.TBD.Enabled = false // the chain must also run for coverage alone
	now := reviewtest.TestNow
	h.Deps.Now = func() time.Time { return now }
	var queued []core.Job
	h.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	h.GL.MR.Pipeline.Status = "running"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Kind != core.JobRefreshStatus {
		t.Fatalf("queued %+v", queued)
	}
	sid := h.GL.CreatedNotes[0]
	h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(sid, h.GL.Notes[sid], nil)}}}
	h.GL.Mu.Lock()
	h.GL.MR.Pipeline.Status = "success"
	h.GL.Mu.Unlock()
	if err := h.R().Handle(context.Background(), queued[0]); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	blk := marker.CoverageRe.FindString(h.GL.Notes[sid])
	if !strings.Contains(blk, "🧪 Покрытие нового кода: 50% (1 из 2 строк)") || strings.Contains(blk, "⏳") || strings.Count(h.GL.Notes[sid], marker.CoverageStart) != 1 {
		t.Errorf("refreshed block:\n%s", blk)
	}
}
