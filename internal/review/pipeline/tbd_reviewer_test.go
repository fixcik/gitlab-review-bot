package pipeline_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

func TestTBDBlockInSummary(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Review.TBD.MaxLines = 3
	h.GL.Diffs = append(h.GL.Diffs,
		core.FileDiff{OldPath: "go.sum", NewPath: "go.sum", Diff: strings.Repeat("+x\n", 100)}, // default_ignore
		core.FileDiff{OldPath: "big.go", NewPath: "big.go", Diff: ""},                          // collapsed by GitLab
		core.FileDiff{OldPath: "old.go", NewPath: "old.go", Diff: "@@ -1,2 +1,1 @@\n-a\n-b\n+c\n"},
	)
	h.GL.Commits = []core.Commit{{SHA: "a", AuthoredAt: reviewtest.TestNow.Add(-5 * 24 * time.Hour)}, {SHA: "b", AuthoredAt: reviewtest.TestNow.Add(-time.Hour)}}
	h.GL.MR.DivergedCommits = 45
	h.GL.MR.Pipeline = &core.Pipeline{Status: "failed", SHA: "head111111111111", WebURL: "https://gitlab.example.com/g/api/-/pipelines/9"}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sum := reviewtest.SummaryBody(t, h)
	blk := marker.TBDRe.FindString(sum)
	for _, want := range []string{
		"### 🌿 Trunk-based",
		"- 📏 ≥ +3 / −2 строк, 3 файла — ⚠️ больше 3: такой MR трудно ревьюить",
		"- 🕒 ветке 5 дней — ⚠️ дольше 3 дня",
		"- ↩️ отстаёт от `main` на 45 коммитов — ⚠️ больше 30: обновите ветку (rebase)",
		"- ❌ [пайплайн упал](https://gitlab.example.com/g/api/-/pipelines/9) — main должен оставаться зелёным",
	} {
		if !strings.Contains(blk, want) {
			t.Errorf("block lacks %q:\n%s", want, blk)
		}
	}
	if strings.Index(sum, marker.TBDStart) > strings.Index(sum, summary.CompatSectionTitle) && strings.Contains(sum, summary.CompatSectionTitle) {
		t.Error("the block must come before the compat section")
	}
}

func TestTBDQuietAndDisabled(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.Commits = []core.Commit{{SHA: "a", AuthoredAt: reviewtest.TestNow.Add(-3 * time.Hour)}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	blk := marker.TBDRe.FindString(reviewtest.SummaryBody(t, h))
	if strings.Contains(blk, "⚠️") || !strings.Contains(blk, "- 📏 +2 / −0 строки, 1 файл") ||
		!strings.Contains(blk, "- 🕒 ветке 3 часа") || !strings.Contains(blk, "- ↩️ ветка актуальна относительно `main`") ||
		!strings.Contains(blk, "- ⚪ пайплайна нет") {
		t.Errorf("block:\n%s", blk)
	}

	h = reviewtest.New(t)
	h.Cfg.Review.TBD.Enabled = false
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reviewtest.SummaryBody(t, h), "reviewbot:tbd") {
		t.Error("disabled: no block expected")
	}
}

func TestTBDPipelineRefreshChain(t *testing.T) {
	h := reviewtest.New(t)
	now := reviewtest.TestNow
	h.Deps.Now = func() time.Time { return now }
	var queued []core.Job
	h.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	h.GL.MR.Pipeline = &core.Pipeline{Status: "running", SHA: "head111111111111"}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}, {Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Kind != core.JobRefreshStatus || queued[0].MRIID != reviewtest.IID {
		t.Fatalf("queued %+v", queued)
	}
	if !strings.Contains(reviewtest.SummaryBody(t, h), "⏳ пайплайн выполняется") {
		t.Fatal("running pipeline not shown")
	}
	sid := h.GL.CreatedNotes[0]
	sync := func() { // the fake lists the summary note with its current body
		h.GL.Mu.Lock()
		h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(sid, h.GL.Notes[sid], nil)}}}
		h.GL.Mu.Unlock()
	}
	sync()
	// A second review while the chain waits does not start another one.
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobFullReview)); err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("second chain started: %+v", queued)
	}

	// Still running: asks to be retried in two minutes.
	job := queued[0]
	sync()
	err := h.R().Handle(context.Background(), job)
	rl, ok := core.AsRetryLater(err)
	if !ok || !rl.At.Equal(now.Add(pipeline.TBDRefreshEvery)) {
		t.Fatalf("want RetryLater in 2m, got %v", err)
	}
	// Finished: the line is updated and the chain ends.
	h.GL.Mu.Lock()
	h.GL.MR.Pipeline = &core.Pipeline{Status: "success", SHA: "head111111111111"}
	h.GL.Mu.Unlock()
	now = now.Add(4 * time.Minute)
	sync()
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatalf("finished pipeline: %v", err)
	}
	sum := h.GL.Notes[sid]
	if len(h.GL.CreatedNotes) != 1 {
		t.Fatalf("summary notes %v", h.GL.CreatedNotes)
	}
	if !strings.Contains(sum, "✅ пайплайн прошёл") || strings.Contains(sum, "выполняется") || strings.Count(sum, marker.TBDStart) != 1 {
		t.Errorf("summary after refresh:\n%s", marker.TBDRe.FindString(sum))
	}
	if _, ok := h.Internals().Pipeline.Refreshing.StartedAt(job.MRKey()); ok {
		t.Error("chain must end")
	}

	// A pipeline running for hours: the chain gives up.
	h.GL.Mu.Lock()
	h.GL.MR.Pipeline = &core.Pipeline{Status: "running", SHA: "head111111111111"}
	h.GL.Mu.Unlock()
	h.Internals().Pipeline.Refreshing.Start(job.MRKey(), now.Add(-pipeline.TBDRefreshFor-time.Minute))
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatalf("give up: %v", err)
	}
}

func TestTBDFeatureFlagRule(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sys := h.Runner.Reqs[0].SystemPrompt
	for _, want := range []string{"switching off changed behavior", `severity "major"`, "no feature-flag system yet", "Do NOT ask for a switch for additive changes"} {
		if !strings.Contains(sys, want) {
			t.Errorf("rule lacks %q", want)
		}
	}

	h = reviewtest.New(t)
	h.Cfg.Review.TBD.FlagsHint = `flags.Enabled(ctx, "name")`
	h.GL.RepoConfig = "tbd:\n  feature_flags: false\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.Runner.Reqs[0].SystemPrompt, "switching off changed behavior") {
		t.Error("repo turned the rule off")
	}
	if note := pipeline.TBDRulesNote(h.Cfg.Review.TBD); !strings.Contains(note, `How the team switches behavior: flags.Enabled(ctx, "name").`) {
		t.Errorf("hint missing: %s", note)
	}
}
