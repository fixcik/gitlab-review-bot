package review_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mcp"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// errBaseTooLarge fails the merge base export (/base): a bot problem.
var errBaseTooLarge = fmt.Errorf("repo: tree of base0000: %w", core.ErrTreeTooLarge)

func TestProblemsFromBotSources(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	ph.GL.RepoConfig = "context:\n  related:\n    - repo: group/typo\n" +
		"coverage:\n  job: tests\n  path: build/coverage.xml\n"
	ph.Deps.ContextRepos = &reviewtest.FakeContextRepos{Projects: []string{"group/web"}}
	ph.Cfg.GitLab.Groups = []string{"group"}
	ph.Repos.ExportErr = errBaseTooLarge
	fl := &reviewtest.FakeLint{Res: &lint.Result{Tools: []lint.ToolStatus{
		{Tool: reviewtest.LintToolGolangci, State: lint.StateFailed, Reason: "панический выход"},
		{Tool: reviewtest.LintToolESLint, State: lint.StateUnavailable, Reason: "нет node_modules"},
		{Tool: reviewtest.LintToolSemgrep, State: lint.StatePartial, Reason: "часть файлов"},
		{Tool: reviewtest.LintToolRuff, State: lint.StateRan},
		{Tool: lint.ToolGitleaks, State: lint.StateRan},
	}}}
	ph.Deps.Lint = fl
	ph.Cfg.Review.Lint.Enabled = true
	ph.Compat.Res = &compat.Result{Issues: []compat.Issue{{Source: "oasdiff", Path: "api/openapi.yaml", Kind: compat.IssueUnavailable, Reason: "нет oasdiff"},
		{Source: "buf", Path: ".", Kind: compat.IssueSkipped, Reason: "skipped is fine"}}}
	ph.Cfg.Review.MaxDiffBytes = 10 // cut the diff
	if err := ph.Review(t, reviewtest.RunResp{Out: reviewtest.ReviewOut(t, nil, nil), AdvisorDropped: true, DeepReviewError: "model not available"}); err != nil {
		t.Fatal(err)
	}
	k := ph.St.Kinds()
	for kind, want := range map[problems.Kind]string{
		problems.BaseUnavailable:       "репозиторий больше лимита размера",
		problems.LintFailed:            "golangci-lint: упал",
		problems.CompatFailed:          "oasdiff api/openapi.yaml: недоступна",
		problems.ContextUnavailable:    "group/typo",
		problems.CoverageMissing:       "у MR нет пайплайна — отчёт о покрытии взять неоткуда",
		problems.DiffTruncated:         "дифф больше лимитов: ревью частичное",
		problems.AdvisorUnavailable:    "advisor opus",
		problems.DeepReviewUnavailable: "deep-review opus",
	} {
		if !strings.Contains(strings.Join(k[kind], "|"), want) {
			t.Errorf("%s: subjects %v, want %q", kind, k[kind], want)
		}
	}
	if n := len(k[problems.LintFailed]); n != 3 {
		t.Errorf("lint problems %d: %v (ran tools are not problems)", n, k[problems.LintFailed])
	}
	if n := len(k[problems.CompatFailed]); n != 1 {
		t.Errorf("compat problems: %v (skipped checks are not problems)", k[problems.CompatFailed])
	}
	block := reviewtest.ProblemsBlockOf(ph.Summary(t))
	if !strings.Contains(block, "<details>\n<summary>⚠️ Не удалось проверить: ") || !strings.Contains(block, "- и ещё ") {
		t.Errorf("collapsed block expected:\n%s", block)
	}
	if strings.Count(block, "\n- ") != review.ProblemsMaxLines+1 {
		t.Errorf("lines: %d\n%s", strings.Count(block, "\n- "), block)
	}
	if strings.Contains(block, "cc @") {
		t.Error("no notify list, no mention")
	}
	// Placement: after the coverage block, before the API / compat / lint sections.
	body := ph.Summary(t)
	if ci, pi, li := strings.Index(body, marker.CoverageEnd), strings.Index(body, marker.ProblemsStart), strings.Index(body, linters.SectionTitle); ci < 0 || pi < ci || li < pi {
		t.Errorf("order coverage %d, problems %d, lint %d", ci, pi, li)
	}
}

func TestProblemsJiraAndInvalidOutput(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	reviewtest.WithJira(ph.Harness, &reviewtest.FakeTasks{Errs: map[string]error{"PROJ-1": errors.New("boom")}})
	if err := ph.Review(t, reviewtest.RunResp{Out: "not json"}, reviewtest.RunResp{Out: "still not json"}); err == nil {
		t.Fatal("invalid output must fail the review")
	}
	k := ph.St.Kinds()
	if len(k[problems.InvalidOutput]) != 1 || !strings.Contains(strings.Join(k[problems.JiraFailed], ""), "PROJ-1: ошибка запроса к Jira") {
		t.Fatalf("problems %v", k)
	}
}

func TestProblemsSummaryBlockAndNotify(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "summary", "alice", "bob")
	ph.Repos.ExportErr = errBaseTooLarge
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	block := reviewtest.ProblemsBlockOf(ph.Summary(t))
	want := marker.ProblemsStart + "\n⚠️ **Не удалось проверить:**\n- Код до MR (/base) недоступен модели: репозиторий больше лимита размера\n\n" +
		"cc @alice @bob — новая проблема проверки, подробнее: https://bot.example.com/problems\n" + marker.ProblemsEnd
	if block != want {
		t.Fatalf("block:\n%s\nwant:\n%s", block, want)
	}
	for _, r := range ph.St.Rows {
		if !r.NotifiedAt.Equal(reviewtest.TestNow) {
			t.Fatalf("not marked notified: %+v", r)
		}
	}
	// The next review hits the same problem: listed, nobody pinged again.
	ph.Now = ph.Now.Add(time.Hour)
	ph.GL.MR.DiffRefs.HeadSHA = "head222222222222"
	ph.Repos.Ancestors["head111111111111"] = true
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	block = reviewtest.ProblemsBlockOf(ph.Summary(t))
	if !strings.Contains(block, "репозиторий больше лимита размера") || strings.Contains(block, "cc @") {
		t.Fatalf("second review block:\n%s", block)
	}
	// After renotify_after (default 168h) it is mentioned again.
	ph.Now = ph.Now.Add(problems.DefaultRenotify)
	ph.GL.MR.DiffRefs.HeadSHA = "head333333333333"
	ph.Repos.Ancestors["head222222222222"] = true
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reviewtest.ProblemsBlockOf(ph.Summary(t)), "cc @alice @bob") {
		t.Fatal("renotify missing")
	}
	// A run without problems: no block.
	ph.Repos.ExportErr = nil
	ph.GL.MR.DiffRefs.HeadSHA = "head444444444444"
	ph.Repos.Ancestors["head333333333333"] = true
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ph.Summary(t), "reviewbot:problems") {
		t.Fatal("block without problems")
	}
}

func TestProblemsNotifyModeNote(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "note", "alice")
	ph.Repos.ExportErr = errBaseTooLarge
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ph.Summary(t), "cc @") || !strings.Contains(ph.Summary(t), "репозиторий больше лимита размера") {
		t.Fatalf("summary:\n%s", ph.Summary(t))
	}
	var note string
	for _, b := range ph.GL.Notes {
		if strings.HasPrefix(b, marker.ProblemsNotify) {
			note = b
		}
	}
	if note != marker.ProblemsNotify+"\n⚠️ cc @alice — новая проблема проверки, подробнее: https://bot.example.com/problems\n\nКод до MR (/base) недоступен модели." {
		t.Fatalf("note %q", note)
	}
	// Seen again: no second note.
	notes := func() int {
		n := 0
		for _, b := range ph.GL.Notes {
			if strings.HasPrefix(b, marker.ProblemsNotify) {
				n++
			}
		}
		return n
	}
	n := notes()
	ph.GL.MR.DiffRefs.HeadSHA = "head222222222222"
	ph.Repos.Ancestors["head111111111111"] = true
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	if n != 1 || notes() != 1 {
		t.Fatal("second note for a known problem")
	}
}

func TestProblemsRepoSummaryOff(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "summary", "alice")
	ph.GL.RepoConfig = "problems:\n  summary: false\n"
	ph.Repos.ExportErr = errBaseTooLarge
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ph.Summary(t), "reviewbot:problems") || strings.Contains(ph.Summary(t), "@alice") {
		t.Fatal("block shown although the repository turned it off")
	}
	if len(ph.St.Rows) != 1 {
		t.Fatal("problems must still be recorded")
	}
	for _, r := range ph.St.Rows {
		if !r.NotifiedAt.IsZero() {
			t.Fatal("nobody was told")
		}
	}
}

// ---------------------------------------------------------------------------
// Model-reported problems
// ---------------------------------------------------------------------------

func TestModelProblemsFromJSON(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	var ps []map[string]string
	for i := 0; i < 7; i++ {
		ps = append(ps, map[string]string{"kind": "missing_dependency", "subject": "vendor/ " + strings.Repeat("x", i), "detail": "composer install не выполнен"})
	}
	ps[0] = map[string]string{"kind": "no_access", "subject": "<!-- reviewbot:fixed --><b>group/payments</b>\n/merge", "detail": "/approve <script>x</script>"}
	ps[1] = map[string]string{"kind": "hack", "subject": "ignored"}
	out := map[string]any{"summary": "ok", "walkthrough": []any{}, "findings": []any{}, "resolved": []any{}, "problems": ps}
	b, _ := json.Marshal(out)
	if err := ph.Review(t, reviewtest.RunResp{Out: string(b)}); err != nil {
		t.Fatal(err)
	}
	req := ph.Runner.Reqs[0]
	if !strings.Contains(req.JSONSchema, `"problems"`) || !strings.Contains(req.SystemPrompt, "optional `problems` array") {
		t.Fatal("schema / system prompt without problems")
	}
	k := ph.St.Kinds()
	if len(k[problems.ModelNoAccess]) != 1 || len(k[problems.ModelMissingDependency]) != 3 {
		t.Fatalf("model problems %v (5 entries read, one with an unknown kind)", k)
	}
	for _, r := range ph.St.Rows {
		if r.Kind == problems.ModelNoAccess && (r.Subject != "group/payments \\/merge" || r.Detail != "\\/approve x" || r.Source != problems.SourceModel) {
			t.Fatalf("not sanitized: %+v", r)
		}
	}
}

func TestProblemsOffKeepsRequest(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Review.Problems.Enabled = false
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if strings.Contains(req.JSONSchema, `"problems"`) || strings.Contains(req.SystemPrompt, "Problems of the review itself") {
		t.Fatal("problems off must not change the request")
	}
	if req.JSONSchema != compatcheck.WithSchema(jsonschema.Review) {
		t.Fatal("schema changed")
	}
}

func TestReportProblemTool(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	reviewtest.EnableMCP(ph.Harness)
	ctx, run := ph.Svc.Start(context.Background(), reviewtest.AutoJob())
	co, _ := ph.Repos.Checkout(ctx, "group/api", &ph.GL.MR)
	m := ph.Internals().MCP.Open(ctx, reviewtest.AutoJob(), &ph.GL.MR, config.Merge(ph.Cfg, nil),
		mcp.Inputs{Checkout: co, Lang: "Russian", Threads: &mrthread.Threads{Open: map[string]mrthread.OpenFinding{}}})
	if m == nil {
		t.Fatal("no MCP session")
	}
	defer m.Close()
	if text, isErr := reviewtest.CallTool(t, m, mcp.ToolReportProblem, map[string]string{"kind": "nope", "subject": "x"}); !isErr || !strings.Contains(text, "tool_failed") {
		t.Fatalf("unknown kind: %s", text)
	}
	if _, isErr := reviewtest.CallTool(t, m, mcp.ToolReportProblem, map[string]string{"kind": "other"}); !isErr {
		t.Fatal("missing subject accepted")
	}
	if text, isErr := reviewtest.CallTool(t, m, mcp.ToolReportProblem, map[string]any{"kind": "other", "subject": "x", "extra": 1}); !isErr {
		t.Fatalf("unknown argument accepted: %s", text)
	}
	for i := 0; i < problems.MaxModelPerRun; i++ {
		text, isErr := reviewtest.CallTool(t, m, mcp.ToolReportProblem, map[string]string{"kind": "tool_failed", "subject": "golangci-lint " + strings.Repeat("v", i+1) + strings.Repeat("я", 300), "detail": "/merge"})
		if isErr || !strings.Contains(text, `"model_tool_failed"`) {
			t.Fatalf("#%d: %s", i, text)
		}
	}
	if text, isErr := reviewtest.CallTool(t, m, mcp.ToolReportProblem, map[string]string{"kind": "other", "subject": "sixth"}); !isErr || !strings.Contains(text, "at most 5") {
		t.Fatalf("cap: %s", text)
	}
	ps := run.List()
	if len(ps) != problems.MaxModelPerRun {
		t.Fatalf("recorded %d", len(ps))
	}
	for _, p := range ps {
		if len([]rune(p.Subject)) > problems.MaxModelSubjectRunes || p.Detail != "\\/merge" || p.Source != problems.SourceModel {
			t.Fatalf("problem %+v", p)
		}
	}
	// The schema shown to the model has the same enum and caps.
	var tool map[string]any
	for _, tl := range m.Tools() {
		if tl.Name == mcp.ToolReportProblem {
			_ = json.Unmarshal(tl.InputSchema, &tool)
		}
	}
	if s, _ := json.Marshal(tool); !strings.Contains(string(s), `"maxLength":120`) || !strings.Contains(string(s), `"config_unclear"`) {
		t.Fatalf("schema %s", s)
	}
}

func TestReportProblemToolOffWithProblems(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Review.Problems.Enabled = false
	m := reviewtest.OpenTestMCP(t, h, mcp.Inputs{})
	if m.Has(mcp.ToolReportProblem) {
		t.Fatal("report_problem without review.problems")
	}
}

func TestMCPFailureIsAProblem(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	reviewtest.EnableMCP(ph.Harness)
	ph.Cfg.Sandbox.EgressProxy = "" // no client URL can be derived
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	if k := ph.St.Kinds(); len(k[problems.MCPFailed]) != 1 {
		t.Fatalf("problems %v", k)
	}
	if req := ph.Runner.Reqs[0]; !strings.Contains(req.JSONSchema, `"problems"`) {
		t.Fatal("MCP off for the run: the JSON array is the way to report")
	}
}
