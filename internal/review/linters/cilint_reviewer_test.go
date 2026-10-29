package linters_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

func TestCILintReportsBecomeThreads(t *testing.T) {
	h, fl, m := reviewtest.CIHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	// gitleaks still runs in the bot; nothing else does.
	if len(fl.Calls) != 1 || strings.Join(fl.Calls[0].Config.Tools, ",") != "gitleaks" {
		t.Fatalf("gitleaks must still run: %+v", fl.Calls)
	}
	th := reviewtest.LintThreads(h)
	if len(th) != 2 {
		t.Fatalf("threads %+v", h.GL.Published)
	}
	byTool := map[string]reviewtest.DraftNote{}
	for _, d := range th {
		_, tool, _ := marker.ParseLint(d.Body)
		byTool[tool] = d
	}
	sg, rf := byTool["ci:semgrep"], byTool["ci:ruff"]
	if sg.Pos == nil || sg.Pos.NewLine != 3 || !strings.Contains(sg.Body, "**🔧 ci:semgrep** · `go.lang.security.audit.dangerous-exec-command` · 🟠 Major") ||
		!strings.Contains(sg.Body, "[Описание правила](https://sg.run/ydO7)") || strings.Contains(sg.Body, marker.Blocking) {
		t.Errorf("semgrep thread (SARIF, runner path mapped, not blocking):\n%s", sg.Body)
	}
	if rf.Pos == nil || rf.Pos.NewLine != 2 || !strings.Contains(rf.Body, "`F401`") || strings.Contains(rf.Body, marker.Blocking) {
		t.Errorf("ruff thread (codequality report of parallel jobs):\n%s", rf.Body)
	}
	if len(m.Resolved) != 2 {
		t.Errorf("non-blocking CI threads are resolved right away: %v", m.Resolved)
	}
	p := h.Runner.Reqs[0].Prompt
	if !strings.Contains(p, "[ci:semgrep go.lang.security.audit.dangerous-exec-command] cache.go:3 (major)") || !strings.Contains(p, "[ci:ruff F401] cache.go:2") {
		t.Errorf("prompt lacks the CI results:\n%s", p)
	}
	sum := reviewtest.SummaryBody(t, h)
	blk := marker.CILintRe.FindString(sum)
	for _, s := range []string{
		"<summary>🔧 Линтеры из CI: 2 замечания на изменённых строках</summary>",
		"| ci:semgrep | [semgrep](https://gitlab.example.com/group/api/-/jobs/1) | 1 на изменённых строках (всего в отчёте: 2) |",
		"| ci:ruff | `ruff 1/2`, `ruff 2/2` | 1 на изменённых строках |",
		"новых тредов: 2.",
		"[#77](https://gitlab.example.com/group/api/-/pipelines/77)",
		"<!-- reviewbot:cilint-pending  -->",
	} {
		if !strings.Contains(blk, s) {
			t.Errorf("CI block lacks %q:\n%s", s, sum)
		}
	}
	if strings.Contains(blk, "⏳") || strings.Index(sum, linters.SectionTitle) > strings.Index(sum, marker.CILintStart) {
		t.Errorf("nothing pending; the CI block follows the lint section:\n%s", sum)
	}
	if !strings.Contains(sum, "<summary>🔧 Линтеры и сканеры: 0 замечаний на изменённых строках</summary>") || !strings.Contains(sum, "| gitleaks | ✅ секретов нет |") {
		t.Errorf("gitleaks section:\n%s", sum)
	}
}

func TestCILintBlockingEntry(t *testing.T) {
	h, _, _ := reviewtest.CIHarness(t)
	h.Cfg.Review.BlockMinSeverity = "major"
	h.GL.RepoConfig = "lint:\n  ci:\n    - job: semgrep\n      artifact: gl-sast.sarif\n      blocking: true\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	th := reviewtest.LintThreads(h)
	if len(th) != 1 || !strings.Contains(th[0].Body, marker.Blocking) {
		t.Fatalf("a blocking entry's major result blocks with block_min_severity=major: %+v", th)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "⛔ **Мерж заблокирован:** 1 блокирующее") {
		t.Errorf("gate:\n%s", sum)
	}
}

// After the pipeline, a blocking CI result updates the merge-gate line.
func TestCILintRefreshUpdatesGate(t *testing.T) {
	h, _, _ := reviewtest.CIHarness(t)
	h.Cfg.Review.BlockMinSeverity = "major"
	h.GL.RepoConfig = "lint:\n  ci:\n    - job: semgrep\n      artifact: gl-sast.sarif\n      blocking: true\n"
	var queued []core.Job
	h.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	h.GL.Jobs[0].Status = "running"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sid := h.GL.CreatedNotes[0]
	if !strings.Contains(h.GL.Notes[sid], summary.GateClearLine) {
		t.Fatalf("no blocking thread yet:\n%s", h.GL.Notes[sid])
	}
	h.GL.Mu.Lock()
	h.GL.Jobs[0].Status = "success"
	h.GL.Discussions = append(h.GL.Discussions, core.Discussion{ID: "s", Notes: []core.Note{reviewtest.BotNote(sid, h.GL.Notes[sid], nil)}})
	h.GL.Mu.Unlock()
	if err := h.R().Handle(context.Background(), queued[0]); err != nil {
		t.Fatal(err)
	}
	if sum := h.GL.Notes[sid]; !strings.Contains(sum, "⛔ **Мерж заблокирован:** 1 блокирующее") {
		t.Errorf("gate after the refresh:\n%s", sum)
	}
}

func TestCILintFailures(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	ph.Cfg.Review.Lint.Enabled = true
	ph.Deps.Lint = &reviewtest.FakeLint{Res: &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}}
	ph.GL.RepoConfig = `lint:
  ci:
    - job: phpstan
      artifact: phpstan.json
    - job: eslint
      artifact: eslint.json
    - job: missing-job
      artifact: x.json
    - job: manual
      report: codequality
    - job: garbage
      artifact: g.json
`
	ph.GL.MR.Pipeline = &core.Pipeline{ID: 9, Status: "failed", SHA: "head111111111111"}
	ph.GL.Jobs = []core.CIJob{
		{ID: 1, Name: "phpstan", Status: "failed"}, // failed with a log, no report
		{ID: 2, Name: "eslint", Status: "success"}, // success, file missing
		{ID: 3, Name: "manual", Status: "manual"},
		{ID: 4, Name: "garbage", Status: "success"},
	}
	ph.GL.Artifacts = map[string][]byte{"4:g.json": []byte("Fatal error: out of memory")}
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	blk := marker.CILintRe.FindString(ph.Summary(t))
	for _, s := range []string{
		"нет отчёта: 5",
		"⚠️ ci:phpstan: job `phpstan` упала, файла `phpstan.json` нет.",
		"⚠️ ci:eslint: в артефактах job `eslint` нет файла `eslint.json` (или артефакты истекли).",
		"⚠️ ci:missing-job: job `missing-job` не найдена в пайплайне MR.",
		"⚠️ ci:manual: job `manual` не выполнялась (ждёт ручного запуска).",
		"⚠️ ci:garbage: не удалось разобрать отчёт",
	} {
		if !strings.Contains(blk, s) {
			t.Errorf("block lacks %q:\n%s", s, blk)
		}
	}
	got := strings.Join(ph.St.Kinds()[problems.LintFailed], "|")
	for _, s := range []string{"ci:phpstan: упал", "ci:eslint: упал", "ci:missing-job: недоступен", "ci:manual: недоступен", "ci:garbage: упал"} {
		if !strings.Contains(got, s) {
			t.Errorf("problems lack %q: %s", s, got)
		}
	}
}

// A job that finished without a report after the review (refresh) is a
// problem once; nothing that was already final is recorded again.
func TestCILintRefreshRecordsNewFailuresOnce(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	ph.Cfg.Review.Lint.Enabled = true
	ph.Cfg.Review.TBD.Enabled = false
	ph.Cfg.Review.CI.Enabled = false // the CI digest keeps the chain while the pipeline runs (pipeline/ci.go)
	ph.Deps.Lint = &reviewtest.FakeLint{Res: &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}}
	var queued []core.Job
	ph.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	ph.GL.RepoConfig = "lint:\n  ci:\n    - job: phpstan\n      artifact: phpstan.json\n    - job: gone\n      artifact: x.json\n"
	ph.GL.MR.Pipeline = &core.Pipeline{ID: 9, Status: "running", SHA: "head111111111111"}
	ph.GL.Jobs = []core.CIJob{{ID: 1, Name: "phpstan", Status: "running"}}
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	if got := ph.St.Kinds()[problems.LintFailed]; len(got) != 1 || !strings.Contains(got[0], "ci:gone") {
		t.Fatalf("review: only the missing job is a problem: %v", got)
	}
	ph.St.Rows = map[string]*problems.Record{}
	sid := ph.GL.CreatedNotes[0]
	ph.GL.Mu.Lock()
	ph.GL.Jobs[0].Status = "failed"
	ph.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(sid, ph.GL.Notes[sid], nil)}}}
	ph.GL.Mu.Unlock()
	if err := ph.Svc.Handler(ph.R().Handle)(context.Background(), queued[0]); err != nil {
		t.Fatal(err)
	}
	if got := ph.St.Kinds()[problems.LintFailed]; len(got) != 1 || !strings.Contains(got[0], "ci:phpstan: упал") {
		t.Errorf("refresh: the new failure only: %v", got)
	}
	if blk := marker.CILintRe.FindString(ph.GL.Notes[sid]); !strings.Contains(blk, "job `phpstan` упала, файла `phpstan.json` нет") {
		t.Errorf("block:\n%s", blk)
	}
}

// A ci: thread closes when the report for a newer head was read and no
// longer has the result; a job that did not produce a report closes nothing.
func TestCILintFixedThreadClosed(t *testing.T) {
	ruff := lint.Finding{Tool: "ci:ruff", Rule: "F401", Path: "cache.go", Line: 2, Severity: core.SeverityMinor, Message: "`os` imported but unused", InDiff: true}
	sg := lint.Finding{Tool: "ci:semgrep", Rule: "go.lang.security.audit.dangerous-exec-command", Path: "cache.go", Line: 3, Severity: core.SeverityMajor,
		Message: "Detected non-static command inside Command", InDiff: true, MayBlock: true}
	h, _, m := reviewtest.CIHarness(t)
	h.GL.Discussions = []core.Discussion{
		{ID: "d-ruff", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1, linters.NoteBody(ruff.ID(), ruff, false, false, false), &core.Position{NewPath: "cache.go", NewLine: 2})}},
		{ID: "d-sg", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(2, linters.NoteBody(sg.ID(), sg, true, false, false), &core.Position{NewPath: "cache.go", NewLine: 3})}},
		{ID: "d-other", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(3, linters.NoteBody("0123456789ab", lint.Finding{Tool: "ci:eslint", Rule: "r", Path: "cache.go", Line: 2}, false, false, false), nil)}},
	}
	// ruff's report no longer has F401; semgrep's job failed without a
	// report; eslint is not configured any more.
	h.GL.Reports = map[string][]byte{"2:codequality": []byte("[]"), "3:codequality": []byte("[]")}
	h.GL.Jobs[0].Status = "failed"
	h.GL.Artifacts = nil
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.Resolved, ","); got != "d-ruff" {
		t.Fatalf("resolved %q: only the thread whose report was read", got)
	}
	r := reviewtest.LastReply(t, h.GL)
	if r.DiscussionID != "d-ruff" || !strings.Contains(r.Body, "в отчёте ci:ruff из CI для последнего коммита этого замечания больше нет") {
		t.Errorf("reply %+v", r)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(marker.CILintRe.FindString(sum), "закрыто как исправленные: 1") || !strings.Contains(sum, "⛔ **Мерж заблокирован:** 1 блокирующее") {
		t.Errorf("summary:\n%s", sum)
	}
}

// A CI result that repeats an open Claude finding (same line, names the
// rule) is not posted again.
func TestCILintSkipsOpenClaudeDuplicate(t *testing.T) {
	h, _, _ := reviewtest.CIHarness(t)
	h.GL.Discussions = []core.Discussion{{ID: "d-claude", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1,
		finding.NoteBody("abc123abc123", output.Finding{Path: "cache.go", Line: 2, Severity: "minor", Title: "F401: неиспользуемый импорт os", Body: "Уберите импорт."}, false, 0),
		&core.Position{NewPath: "cache.go", NewLine: 2})}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	for _, d := range reviewtest.LintThreads(h) {
		if strings.Contains(d.Body, "ci:ruff") {
			t.Fatalf("duplicate of the open Claude thread posted:\n%s", d.Body)
		}
	}
	if blk := marker.CILintRe.FindString(reviewtest.SummaryBody(t, h)); !strings.Contains(blk, "уже обсуждаются: 1") {
		t.Errorf("block:\n%s", blk)
	}
}

func TestCILintNoPipelineAndOldHead(t *testing.T) {
	h, _, _ := reviewtest.CIHarness(t)
	h.GL.MR.Pipeline = nil
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if blk := marker.CILintRe.FindString(reviewtest.SummaryBody(t, h)); !strings.Contains(blk, "у MR нет пайплайна") {
		t.Errorf("no pipeline:\n%s", blk)
	}

	h, _, _ = reviewtest.CIHarness(t)
	var queued []core.Job
	h.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	h.GL.MR.Pipeline.SHA = "old000000000000"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if blk := marker.CILintRe.FindString(reviewtest.SummaryBody(t, h)); !strings.Contains(blk, "⏳ ждём job `semgrep`, `ruff*`") || len(queued) != 1 {
		t.Errorf("pipeline of an older commit: wait (%d queued):\n%s", len(queued), blk)
	}
}

func TestCILintRemovedRepoKeys(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	h.GL.RepoConfig = "lint:\n  custom:\n    - name: arch\n      command: go-arch-lint check\n"
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if len(fl.Calls) != 0 || len(h.Runner.Reqs) != 0 {
		t.Fatal("a removed key is a configuration error: no review")
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "lint.custom") || !strings.Contains(r.Body, "linters now come from CI reports") {
		t.Errorf("reply %+v", r)
	}
}

// A too-big MR (no Claude run) does not read the CI linter reports: only
// gitleaks runs, nothing waits for CI jobs.
func TestCILintNotReadForTooBigMR(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Compat.Res = nil
	h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h.GL.RepoConfig = "lint:\n  ci:\n    - job: semgrep\n      artifact: gl-sast.sarif\n"
	h.GL.MR.Pipeline = &core.Pipeline{ID: 7, Status: "running", SHA: "head111111111111"}
	h.GL.Jobs = []core.CIJob{{ID: 1, Name: "semgrep", Status: "running"}}
	var queued []core.Job
	h.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	h.Cfg.Review.TBD.Enabled = false
	h.Cfg.Review.CI.Enabled = false // the CI digest watches running pipelines (pipeline/ci.go)
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || strings.Contains(h.Summary(), marker.CILintStart) || len(queued) != 0 {
		t.Fatalf("too-big MR: no CI block, no refresh chain (queued %d):\n%s", len(queued), h.Summary())
	}
}
