package linters_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// The config package validates tool names without importing internal/lint:
// both lists must stay the same, and every CI linter non-blocking unless
// its lint.ci entry is marked blocking.
func TestLintToolListsInSync(t *testing.T) {
	if got, want := strings.Join(config.LintTools, ","), strings.Join(lint.AllTools, ","); got != want {
		t.Fatalf("config.LintTools %s != lint.AllTools %s", got, want)
	}
	for _, tool := range []string{reviewtest.LintToolGolangci, reviewtest.LintToolRuff, reviewtest.LintToolESLint, reviewtest.LintToolPHPStan, reviewtest.LintToolPHPCS} {
		if !lint.IsLinter(tool) {
			t.Errorf("%s must be a (non-blocking) linter", tool)
		}
	}
	if lint.IsLinter(lint.ToolGitleaks) {
		t.Error("gitleaks is not a linter")
	}
	var r struct{ lint *linters.Feature } // the zero Reviewer's linters feature
	if r.lint.ResultBlocking(lint.Finding{Tool: reviewtest.LintToolPHPStan, Rule: "phpstan.parse", Severity: core.SeverityCritical}) {
		t.Error("a critical CI linter result must not block the merge unless the entry is blocking")
	}
}

const leakedValue = "AKIAZ7Q3XSUPERSECRET42"

func TestLintResultsBecomeThreads(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	m := reviewtest.WithModerator(h)
	fl.Res = &lint.Result{
		Findings: []lint.Finding{reviewtest.SecretFinding, reviewtest.SemgrepFinding, reviewtest.RuffFinding},
		Tools:    reviewtest.Ran(lint.ToolGitleaks, reviewtest.LintToolSemgrep, reviewtest.LintToolRuff),
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(fl.Calls) != 1 || fl.Calls[0].Base != "base000000000000" || fl.Calls[0].Head != "head111111111111" || fl.Calls[0].Diff == "" {
		t.Fatalf("lint must run against the merge base with the review diff: %+v", fl.Calls)
	}
	th := reviewtest.LintThreads(h)
	if len(th) != 3 {
		t.Fatalf("published: %+v", h.GL.Published)
	}
	byTool := map[string]reviewtest.DraftNote{}
	for _, d := range th {
		_, tool, _ := marker.ParseLint(d.Body)
		byTool[tool] = d
	}
	sec := byTool[lint.ToolGitleaks]
	for _, s := range []string{marker.Blocking, "🔑 🔧 gitleaks", "`aws-access-token`", "🔴 Critical", "секрет нужно отозвать и удалить из истории", "коммит `0123456789ab`", "Значение секрета намеренно не показывается"} {
		if !strings.Contains(sec.Body, s) {
			t.Errorf("secret thread lacks %q:\n%s", s, sec.Body)
		}
	}
	if sec.Pos == nil || sec.Pos.NewLine != 2 {
		t.Errorf("secret must be inline: %+v", sec.Pos)
	}
	sg := byTool[reviewtest.LintToolSemgrep]
	for _, s := range []string{"**🔧 ci:semgrep** · `go.lang.security.audit.dangerous-exec-command` · 🟠 Major", "[Описание правила](https://sg.run/ydO7)", "Не блокирует мерж"} {
		if !strings.Contains(sg.Body, s) {
			t.Errorf("semgrep thread lacks %q:\n%s", s, sg.Body)
		}
	}
	if strings.Contains(sg.Body, marker.Blocking) {
		t.Error("major semgrep result must not block with block_min_severity=critical")
	}
	if rf := byTool[reviewtest.LintToolRuff]; !strings.Contains(rf.Body, "🟡 Minor") || !strings.Contains(rf.Body, "docs.astral.sh") || strings.Contains(rf.Body, marker.Blocking) {
		t.Errorf("ruff thread:\n%s", rf.Body)
	}
	// Non-blocking lint threads are resolved with the service token, the
	// secret stays open.
	if len(m.Resolved) != 2 {
		t.Errorf("resolved: %v", m.Resolved)
	}
	for _, id := range m.Resolved {
		for _, d := range h.GL.Discussions {
			if d.ID == id && strings.Contains(d.Notes[0].Body, "gitleaks") {
				t.Error("secret thread must never be auto-resolved")
			}
		}
	}
	sum := h.GL.Notes[h.GL.CreatedNotes[0]]
	gate := strings.Index(sum, "⛔ **Мерж заблокирован:** 1 блокирующее замечание")
	secrets := strings.Index(sum, "### 🔑 Найдены секреты: 1")
	compatSec := strings.Index(sum, summary.CompatSectionTitle)
	// CI linter results are counted in their own block (cilint_reviewer_test.go).
	lintSec := strings.Index(sum, "<summary>🔧 Линтеры и сканеры: 1 замечание на изменённых строках</summary>")
	if gate < 0 || secrets < 0 || lintSec < 0 || !(gate < secrets && secrets < compatSec && compatSec < lintSec) {
		t.Fatalf("summary layout (gate, secrets first, compat, lint):\n%s", sum)
	}
	for _, s := range []string{"| gitleaks | 🔑 секретов: 1 |", "новых тредов: 1.", "`cache.go:2` — `aws-access-token` (коммит `0123456789ab`)"} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
}

func TestLintSecretNeverLeaks(t *testing.T) {
	// Even if a tool message carried the value (it cannot: the parser never
	// reads it), the prompt and the summary only show rule, location and
	// commit.
	h, fl := reviewtest.LintHarness(t)
	f := reviewtest.SecretFinding
	f.Message = "AWS key " + leakedValue
	f.InDiff = false
	f.Line = 40
	fl.Res = &lint.Result{Findings: []lint.Finding{f}, Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	th := reviewtest.LintThreads(h)
	if len(th) != 1 || th[0].Pos != nil || !strings.Contains(th[0].Body, "не в итоговом диффе") || !strings.Contains(th[0].Body, marker.Blocking) {
		t.Fatalf("a secret only in the history is still a blocking (general) thread: %+v", th)
	}
	if strings.Contains(th[0].Body, leakedValue) || strings.Contains(th[0].Body, "AWS key") {
		t.Errorf("secret thread carries tool text:\n%s", th[0].Body)
	}
	if p := h.Runner.Reqs[0].Prompt; strings.Contains(p, "AWS key") || !strings.Contains(p, "leaked secret (value hidden)") {
		t.Errorf("prompt must not carry the gitleaks message:\n%s", p)
	}
	if sum := reviewtest.SummaryBody(t, h); strings.Contains(sum, leakedValue) {
		t.Errorf("summary leaks the value:\n%s", sum)
	}
}

func TestLintPromptContainsResults(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.SemgrepFinding}, Tools: append(reviewtest.Ran(reviewtest.LintToolSemgrep), lint.ToolStatus{Tool: reviewtest.LintToolRuff, State: lint.StateFailed, Reason: "x"})}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	p := h.Runner.Reqs[0].Prompt
	for _, s := range []string{"## Linter and security scanner results", "Do NOT report the same problems again", "<untrusted_tool_results>", "[ci:semgrep go.lang.security.audit.dangerous-exec-command] cache.go:3 (major)", "These tools could not run this time: ci:ruff"} {
		if !strings.Contains(p, s) {
			t.Errorf("prompt lacks %q:\n%s", s, p)
		}
	}
}

func TestLintClaudeDuplicateDropped(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.SemgrepFinding, reviewtest.SecretFinding}, Tools: reviewtest.Ran(reviewtest.LintToolSemgrep, lint.ToolGitleaks)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 3, Severity: "major", Title: "dangerous-exec-command: команда собирается из ввода", Body: "как и пишет semgrep"},
		{Path: "cache.go", Line: 2, Severity: "critical", Title: "Захардкожен AWS-токен", Body: "уберите ключ"},
		{Path: "cache.go", Line: 3, Severity: "major", Title: "Нет таймаута у exec", Body: "процесс может зависнуть навсегда"},
	}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	var claude []string
	for _, d := range h.GL.Published {
		if _, ok := marker.ParseFinding(d.Body); ok {
			claude = append(claude, d.Body)
		}
	}
	if len(claude) != 1 || !strings.Contains(claude[0], "Нет таймаута") {
		t.Fatalf("only the deeper Claude finding must stay: %v", claude)
	}
}

func TestLintDedupeAcrossReviews(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	h.GL.PublishMakesThreads = true
	fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.SecretFinding, reviewtest.RuffFinding}, Tools: reviewtest.Ran(lint.ToolGitleaks, reviewtest.LintToolRuff)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(reviewtest.LintThreads(h)); n != 2 {
		t.Fatalf("first review: %d", n)
	}
	// Second review: same results, the ruff one moved down a line.
	h.GL.MR.DiffRefs.HeadSHA = "head2"
	moved := reviewtest.RuffFinding
	moved.Line = 4
	fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.SecretFinding, moved}, Tools: reviewtest.Ran(lint.ToolGitleaks, reviewtest.LintToolRuff)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	before := len(h.GL.Published)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobFullReview)); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Published) != before {
		t.Fatalf("lint threads reposted: %+v", h.GL.Published[before:])
	}
	sum := h.GL.Notes[h.GL.CreatedNotes[len(h.GL.CreatedNotes)-1]] // the second run's summary
	if !strings.Contains(sum, "⛔ **Мерж заблокирован:** 1 блокирующее") || !strings.Contains(sum, "уже обсуждаются: 1") {
		t.Errorf("open secret thread still blocks:\n%s", sum)
	}
}

func TestLintResolvedByHumanNotReposted(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	id := reviewtest.RuffFinding.ID()
	h.GL.Discussions = []core.Discussion{{ID: "d-r", Resolvable: true, Resolved: true,
		Notes: []core.Note{reviewtest.BotNote(1, linters.NoteBody(id, reviewtest.RuffFinding, false, true, false), &core.Position{NewPath: "cache.go", NewLine: 3})}}}
	fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.RuffFinding}, Tools: reviewtest.Ran(reviewtest.LintToolRuff)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(reviewtest.LintThreads(h)); n != 0 {
		t.Fatalf("a resolved (non-blocking) lint thread must not be reposted: %d", n)
	}
}

func TestLintAutoCloseOnlyWhenToolRan(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	m := reviewtest.WithModerator(h)
	pos := &core.Position{NewPath: "cache.go", OldPath: "cache.go", NewLine: 2}
	h.GL.Discussions = []core.Discussion{
		{ID: "d-secret", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1, linters.NoteBody(reviewtest.SecretFinding.ID(), reviewtest.SecretFinding, true, false, false), pos)}},
		{ID: "d-semgrep", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(2, linters.NoteBody(reviewtest.SemgrepFinding.ID(), withSeverity(reviewtest.SemgrepFinding, core.SeverityCritical), true, false, false), pos)}},
		{ID: "d-ruff", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(3, linters.NoteBody(reviewtest.RuffFinding.ID(), reviewtest.RuffFinding, false, false, false), pos)}},
	}
	// gitleaks ran and no longer finds the secret; semgrep was unavailable;
	// ruff ran only partially (and neither is in lint.ci: their threads
	// stay anyway).
	fl.Res = &lint.Result{Tools: []lint.ToolStatus{
		{Tool: lint.ToolGitleaks, State: lint.StateRan},
		{Tool: reviewtest.LintToolSemgrep, State: lint.StateUnavailable, Reason: "semgrep не установлен в sandbox-образе"},
		{Tool: reviewtest.LintToolRuff, State: lint.StatePartial, Reason: "проверены первые 500 изменённых файлов"},
	}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.Resolved, ","); got != "d-secret" {
		t.Fatalf("resolved %q: only threads of a tool that ran completely may close", got)
	}
	r := reviewtest.LastReply(t, h.GL)
	if r.DiscussionID != "d-secret" || !strings.Contains(r.Body, marker.Fixed) || !strings.Contains(r.Body, "его всё равно нужно отозвать") {
		t.Errorf("reply: %+v", r)
	}
	sum := reviewtest.SummaryBody(t, h)
	for _, s := range []string{"⛔ **Мерж заблокирован:** 1 блокирующее", "закрыто как исправленные: 1"} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
}

func withSeverity(f lint.Finding, s core.Severity) lint.Finding { f.Severity = s; return f }

func TestLintSemgrepBlockingBySeverity(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	h.Cfg.Review.BlockMinSeverity = "major"
	lf := reviewtest.RuffFinding
	lf.Severity = core.SeverityCritical // overridden, still a linter
	fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.SemgrepFinding, lf}, Tools: reviewtest.Ran(reviewtest.LintToolSemgrep, reviewtest.LintToolRuff)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	for _, d := range reviewtest.LintThreads(h) {
		_, tool, _ := marker.ParseLint(d.Body)
		if blocks := strings.Contains(d.Body, marker.Blocking); blocks != (tool == reviewtest.LintToolSemgrep) {
			t.Errorf("%s blocking=%v:\n%s", tool, blocks, d.Body)
		}
	}
}

func TestLintRepoDisable(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	h.GL.RepoConfig = "lint:\n  disable: [gitleaks]\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fl.Calls[0].Config.Tools, ",")
	if got != "gitleaks" {
		t.Fatalf("tools %q: gitleaks cannot be disabled by the repo", got)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "allow\\_disable\\_secrets") && !strings.Contains(sum, "allow_disable_secrets") {
		t.Errorf("summary must say gitleaks was kept:\n%s", sum)
	}
}

func TestLintRepoDisableSecretsWhenAllowed(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	h.Cfg.Review.Lint.AllowDisableSecrets = true
	h.GL.RepoConfig = "lint:\n  disable: [gitleaks]\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(fl.Calls) != 0 {
		t.Fatalf("nothing to run (no gitleaks, no lint.ci): %+v", fl.Calls)
	}
}

func TestLintDisabled(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	h.Cfg.Review.Lint.Enabled = false
	h.Cfg.Review.Todo.Enabled = false // runs on its own otherwise (todo_reviewer_test.go)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(fl.Calls) != 0 || strings.Contains(reviewtest.SummaryBody(t, h), "Линтеры") || strings.Contains(h.Runner.Reqs[0].Prompt, "Linter and security") {
		t.Fatal("lint must not run")
	}
}

func TestLintCheckErrorStillReviews(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	fl.Err = fmt.Errorf("git exploded")
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "🔧 Линтеры и сканеры: проверки не выполнены") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestLintDefaultCheckerMissingTools(t *testing.T) {
	// No injected checker: the real lint.Checker runs over the fake runner,
	// whose Exec answers 127 for everything. The review goes on and the
	// summary lists the unavailable checks.
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, "🔧 Линтеры и сканеры: 0 замечаний") || !strings.Contains(sum, "Проверка gitleaks недоступна") {
		t.Errorf("summary:\n%s", sum)
	}
}

// When the lint step is switched off (review.lint.enabled: false) the earlier
// open blocking lint threads stay (a leaked secret still blocks the merge in
// GitLab), so the summary's gate line must keep counting them.
func TestLintOffStillCountsOpenBlockingThreads(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	h.Cfg.Review.Lint.Enabled = false
	h.GL.Discussions = []core.Discussion{{ID: "d-secret", Resolvable: true,
		Notes: []core.Note{reviewtest.BotNote(1, linters.NoteBody(reviewtest.SecretFinding.ID(), reviewtest.SecretFinding, true, false, false), nil)}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(fl.Calls) != 0 {
		t.Fatalf("lint must not run when off")
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "Мерж заблокирован:** 1") {
		t.Fatalf("open blocking lint thread not counted:\n%s", sum)
	}
}

// A Claude finding is never dropped in favour of a weaker lint result, and
// it must name the rule or the tool as a whole word (generic tool names
// like the default job name "lint" never count).
func TestLintDuplicateNeverDropsStrongerFinding(t *testing.T) {
	h := reviewtest.New(t) // block_min_severity: critical
	todo := lint.Finding{Tool: lint.ToolTodo, Rule: "TODO", Path: "auth.go", Line: 41, Severity: core.SeverityMinor,
		Message: "TODO без задачи — «TODO: check token expiry»", InDiff: true}
	crit := output.Finding{Path: "auth.go", Line: 40, EndLine: 45, Severity: core.SeverityCritical, Title: "Expired tokens are accepted",
		Body: "The TODO left here means expiry is never checked: any expired JWT authenticates."}
	if out := linters.DropDuplicates([]output.Finding{crit}, &linters.Run{Res: &lint.Result{Findings: []lint.Finding{todo}}}, h.Internals().Lint); len(out) != 1 {
		t.Error("critical finding dropped as a duplicate of a minor todo result")
	}
	minorTodo := crit
	minorTodo.Severity, minorTodo.Title = core.SeverityMinor, "TODO без задачи"
	if linters.DuplicateOf(minorTodo, []lint.Finding{todo}, h.Internals().Lint) == nil {
		t.Error("a genuine todo duplicate must still be dropped")
	}

	ciLint := lint.Finding{Tool: "ci:lint", Rule: "unused", Path: "pay.go", Line: 10, Severity: core.SeverityMinor, Message: "x is unused", InDiff: true}
	f2 := output.Finding{Path: "pay.go", Line: 10, Severity: core.SeverityCritical, Title: "Refund amount ignored",
		Body: "The computed refund is unused, so customers are charged twice."}
	if linters.DuplicateOf(f2, []lint.Finding{ciLint}, h.Internals().Lint) != nil {
		t.Error("critical finding dropped as a duplicate of a minor ci:lint result")
	}
	f2.Severity = core.SeverityMinor
	if linters.DuplicateOf(f2, []lint.Finding{ciLint}, h.Internals().Lint) == nil {
		t.Error("same severity, rule named: a duplicate")
	}
	// Whole words only; the generic tool name "lint" does not count.
	for _, text := range []string{"unused_refund is wrong", "the lint job is right here", "x-unused-y"} {
		g := output.Finding{Path: "pay.go", Line: 10, Severity: core.SeverityMinor, Title: text}
		if linters.DuplicateOf(g, []lint.Finding{ciLint}, h.Internals().Lint) != nil {
			t.Errorf("%q matched", text)
		}
	}

	// Same severity, but the finding blocks the merge and the CI result
	// (not marked blocking) does not: the finding stays.
	sast := lint.Finding{Tool: "ci:semgrep", Rule: "sql-injection", Path: "db.go", Line: 5, Severity: core.SeverityCritical, InDiff: true}
	f3 := output.Finding{Path: "db.go", Line: 5, Severity: core.SeverityCritical, Title: "sql-injection in the query builder"}
	if linters.DuplicateOf(f3, []lint.Finding{sast}, h.Internals().Lint) != nil {
		t.Error("a blocking finding dropped for a non-blocking result")
	}
	if linters.DuplicateOf(f3, []lint.Finding{sast}, nil) == nil {
		t.Error("without the gate: severities equal, rule named -> duplicate")
	}
	sast.MayBlock = true
	if linters.DuplicateOf(f3, []lint.Finding{sast}, h.Internals().Lint) == nil {
		t.Error("both blocking: a duplicate")
	}
}

// End to end: the critical finding next to an added TODO is published.
func TestTodoDoesNotSwallowCriticalFinding(t *testing.T) {
	h := todoHarness(t, "// TODO: rework cache")
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 2, Severity: "critical", Title: "Кэш отдаёт чужие данные", Body: "из-за этого TODO ключ не учитывает пользователя"},
	}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range h.GL.Published {
		if _, ok := marker.ParseFinding(d.Body); ok {
			n++
		}
	}
	if n != 1 || len(todoThreads(h)) != 1 {
		t.Fatalf("claude %d, todo %d: %+v", n, len(todoThreads(h)), h.GL.Published)
	}
}
