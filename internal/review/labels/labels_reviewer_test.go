package labels_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/migrations"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/labels"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// withLabels adds a "labels" field to a review answer.
func withLabels(t *testing.T, out string, labels ...string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	m["labels"] = append([]string{}, labels...)
	return reviewtest.MustJSON(t, m)
}

// ---------------------------------------------------------------------------
// Review flow
// ---------------------------------------------------------------------------

func TestLabelsFullReview(t *testing.T) {
	lh := reviewtest.NewLabelHarness(t)
	lh.Mr.Labels = []string{"team::payments"} // a human's label
	lh.Review(t, reviewtest.AutoJob(), withLabels(t, reviewtest.ReviewOut(t, nil, nil), "type::bug", "not-a-rule"))

	if got := strings.Join(lh.Labels(), ","); got != "backend,team::payments,type::bug" {
		t.Fatalf("labels %s", got)
	}
	// The Reporter bot got 403, the service token applied the change.
	if len(lh.Bot.Updates) != 1 || len(lh.Svc.Updates) != 1 {
		t.Fatalf("updates bot %v svc %v", lh.Bot.Updates, lh.Svc.Updates)
	}
	if got := strings.Join(lh.Ls.Added, ","); got != "backend,type::bug" {
		t.Fatalf("tracked %s", got)
	}
	sum := lh.Summary(t)
	if !strings.Contains(sum, "🏷 Метки: +`backend` +`type::bug`") {
		t.Fatalf("summary line missing:\n%s", sum)
	}
	if got := strings.Join(lh.Metrics, ","); got != "add:ok,add:ok" {
		t.Fatalf("metrics %s", got)
	}
	// The request asked for the instruction labels.
	req := lh.Runner.Reqs[0]
	if !strings.Contains(req.Prompt, "<label_rules>") || !strings.Contains(req.Prompt, `"needs-design-review"`) ||
		strings.Contains(req.Prompt, `"backend"`) || !strings.Contains(req.SystemPrompt, "Merge request labels") {
		t.Fatalf("prompt:\n%s", req.Prompt)
	}
	var schema struct {
		Properties map[string]struct {
			Items struct {
				Enum []string `json:"enum"`
			} `json:"items"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal([]byte(req.JSONSchema), &schema); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(schema.Properties["labels"].Items.Enum, ","); got != "type::bug,needs-design-review" {
		t.Fatalf("enum %q", got)
	}
	if slices.Contains(schema.Required, "labels") {
		t.Fatal("labels must stay optional")
	}

	// Second full review: the bot remembers the 403 and goes straight to
	// the service token; the model drops type::bug, a human removed
	// backend meanwhile (never set again), and nothing else changes.
	lh.Mr.Mu.Lock()
	lh.Mr.Labels = []string{"team::payments", "type::bug"}
	lh.Mr.Mu.Unlock()
	lh.Review(t, reviewtest.ManualJob(core.JobFullReview), withLabels(t, reviewtest.ReviewOut(t, nil, nil)))
	if len(lh.Bot.Updates) != 1 {
		t.Fatalf("the bot token was tried again: %v", lh.Bot.Updates)
	}
	if got := strings.Join(lh.Labels(), ","); got != "team::payments" {
		t.Fatalf("labels after second review %s", got)
	}
	if len(lh.Ls.Added) != 0 || strings.Join(lh.Ls.Suppresed, ",") != "backend" {
		t.Fatalf("state %v %v", lh.Ls.Added, lh.Ls.Suppresed)
	}
	if sum := lh.Summary(t); !strings.Contains(sum, "🏷 Метки: −`type::bug`") {
		t.Fatalf("summary:\n%s", sum)
	}
}

func TestLabelsIncrementalOnlyAddsInstructionLabels(t *testing.T) {
	lh := reviewtest.NewLabelHarness(t)
	lh.Mr.Labels = []string{"type::bug", "backend"}
	lh.Ls.Added = []string{"backend", "type::bug"}
	lh.St.States[core.MRKey(reviewtest.PID, reviewtest.IID)] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old000000000000"}
	lh.Repos.Ancestors["old000000000000"] = true
	// The model sees only the new commits and does not list type::bug.
	lh.Review(t, reviewtest.AutoJob(), withLabels(t, reviewtest.ReviewOut(t, nil, nil), "needs-design-review"))
	if !strings.Contains(lh.Runner.Reqs[0].Prompt, "This is an incremental review: you see only the new commits.") {
		t.Fatal("incremental hint missing")
	}
	if got := strings.Join(lh.Labels(), ","); got != "backend,needs-design-review,type::bug" {
		t.Fatalf("labels %s", got)
	}
	if len(lh.Svc.Updates) != 1 || len(lh.Svc.Updates[0][1]) != 0 {
		t.Fatalf("an incremental review removed an instruction label: %v", lh.Svc.Updates)
	}
}

func TestLabelsHumanLabelsUntouched(t *testing.T) {
	lh := reviewtest.NewLabelHarness(t)
	// needs-design-review was put by a human (not in the bot's record):
	// the model's "no" must not remove it.
	lh.Mr.Labels = []string{"needs-design-review", "backend"}
	lh.Ls.Added = []string{"backend"}
	lh.Repos.Files = []string{"README.md"} // backend no longer applies
	lh.Review(t, reviewtest.AutoJob(), withLabels(t, reviewtest.ReviewOut(t, nil, nil)))
	if got := strings.Join(lh.Labels(), ","); got != "needs-design-review" {
		t.Fatalf("labels %s", got)
	}

	// remove_stale off: the bot's own stale label stays too.
	lh2 := reviewtest.NewLabelHarness(t)
	lh2.GL.RepoConfig = reviewtest.LabelRepoConfig + "  remove_stale: false\n"
	lh2.Mr.Labels = []string{"backend"}
	lh2.Ls.Added = []string{"backend"}
	lh2.Repos.Files = []string{"README.md"}
	lh2.Review(t, reviewtest.AutoJob(), withLabels(t, reviewtest.ReviewOut(t, nil, nil)))
	if got := strings.Join(lh2.Labels(), ","); got != "backend" || len(lh2.Svc.Updates) != 0 {
		t.Fatalf("labels %s updates %v", got, lh2.Svc.Updates)
	}
}

func TestLabelsMissingLabelAndFailures(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	lh := &reviewtest.LabelHarness{Harness: ph.Harness, Mr: &reviewtest.FakeMRLabels{State: "opened", Project: []string{"type::bug"}}}
	lh.Bot = &reviewtest.FakeLabelClient{Mr: lh.Mr, Err: fmt.Errorf("403: %w", core.ErrForbidden)}
	lh.Ls = &reviewtest.LabelFakeStore{FakeStore: ph.Harness.St}
	ph.Deps.LabelStore, ph.Deps.Labels = lh.Ls, lh.Bot // no service token
	ph.GL.RepoConfig = reviewtest.LabelRepoConfig
	if err := ph.Review(t, reviewtest.RunResp{Out: withLabels(t, reviewtest.ReviewOut(t, nil, nil), "type::bug")}); err != nil {
		t.Fatal(err)
	}
	k := ph.St.Kinds()[problems.LabelsFailed]
	sort.Strings(k)
	if got := strings.Join(k, "|"); got != "метки MR: нет прав|метки нет в проекте и его группах: backend" {
		t.Fatalf("problems %q", got)
	}
	sum := ph.Summary(t)
	if !strings.Contains(sum, "🏷 Метки: не удалось: `backend`, `type::bug`") || !strings.Contains(sum, "Метки MR не проставлены") {
		t.Fatalf("summary:\n%s", sum)
	}
	if len(lh.Ls.Added) != 0 {
		t.Fatalf("tracked %v", lh.Ls.Added)
	}

	// create_missing: GitLab creates the label (no project lookup).
	lh2 := reviewtest.NewLabelHarness(t)
	lh2.Mr.Project = nil
	lh2.GL.RepoConfig = reviewtest.LabelRepoConfig + "  create_missing: true\n"
	lh2.Svc.ListErr = fmt.Errorf("must not be called")
	lh2.Review(t, reviewtest.AutoJob(), withLabels(t, reviewtest.ReviewOut(t, nil, nil)))
	if got := strings.Join(lh2.Labels(), ","); got != "backend" {
		t.Fatalf("labels %s", got)
	}

	// GitLab answers 200 but applies nothing: reported as ignored.
	lh3 := reviewtest.NewLabelHarness(t)
	lh3.Bot.Err, lh3.Bot.Ignore, lh3.Svc.Ignore = nil, true, true
	lh3.Review(t, reviewtest.AutoJob(), withLabels(t, reviewtest.ReviewOut(t, nil, nil)))
	if got := strings.Join(lh3.Metrics, ","); got != "add:ignored" || len(lh3.Bot.Updates) != 1 || len(lh3.Svc.Updates) != 1 {
		t.Fatalf("metrics %s, updates %v %v", got, lh3.Bot.Updates, lh3.Svc.Updates)
	}
}

func TestLabelsSkipped(t *testing.T) {
	// Merged in the meantime: nothing is changed.
	lh := reviewtest.NewLabelHarness(t)
	lh.Mr.State = "merged"
	lh.Review(t, reviewtest.AutoJob(), withLabels(t, reviewtest.ReviewOut(t, nil, nil), "type::bug"))
	if len(lh.Svc.Updates)+len(lh.Bot.Updates) != 0 || lh.Ls.Saves != 0 {
		t.Fatal("labels changed on a merged MR")
	}
	// No rules (or labels off in the repo): no label API call at all, and
	// the request carries no labels.
	for _, cfg := range []string{"", reviewtest.LabelRepoConfig + "  enabled: false\n"} {
		lh := reviewtest.NewLabelHarness(t)
		lh.GL.RepoConfig = cfg
		lh.Review(t, reviewtest.AutoJob(), reviewtest.ReviewOut(t, nil, nil))
		if lh.Bot.Reads+lh.Svc.Reads != 0 {
			t.Fatalf("config %q: labels read", cfg)
		}
		req := lh.Runner.Reqs[0]
		if strings.Contains(req.JSONSchema, `"labels"`) || strings.Contains(req.Prompt, "label_rules") {
			t.Fatalf("config %q: labels in the request", cfg)
		}
	}
	// Only deterministic rules: no schema field, no prompt section.
	lh = reviewtest.NewLabelHarness(t)
	lh.GL.RepoConfig = "labels:\n  rules:\n    - name: backend\n      paths: [\"**/*.go\"]\n"
	lh.Review(t, reviewtest.AutoJob(), reviewtest.ReviewOut(t, nil, nil))
	if req := lh.Runner.Reqs[0]; strings.Contains(req.JSONSchema, `"labels"`) || strings.Contains(req.Prompt, "label_rules") {
		t.Fatal("deterministic rules must not change the request")
	}
	if got := strings.Join(lh.Labels(), ","); got != "backend" {
		t.Fatalf("labels %s", got)
	}
}

// ---------------------------------------------------------------------------
// Pure parts
// ---------------------------------------------------------------------------

func TestLabelFacts(t *testing.T) {
	eff := config.Merge(func() *config.Service { c := config.Defaults(); return &c }(), nil)
	in := labels.NewInputs([]string{"internal/x.go", "docs/a.md"}, true)
	f := labels.Facts(eff, in, labels.Run{
		Migrations: &migcheck.Run{Res: &migrations.Result{Files: []migrations.File{{Path: "db/1.sql"}}}},
		Compat:     &compatcheck.Run{Res: &compat.Result{Specs: []compat.Spec{{Kind: "openapi", Path: "api.yaml"}}}},
		Lint:       &linters.Run{Res: &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}},
		TBD:        &pipeline.TBDStatus{SizeKnown: true, Added: 500, Removed: 1},
	})
	want := map[string]labels.Tri{config.FactMigrations: labels.TriYes, config.FactAPIChanged: labels.TriYes, config.FactAPIBreaking: labels.TriNo,
		config.FactBigMR: labels.TriYes, config.FactLintSecret: labels.TriNo, config.FactNoTestsChanged: labels.TriYes}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %v, want %v", k, f[k], v)
		}
	}
	// Failed / off steps stay unknown; breaking from a checker or an
	// oasdiff ERR item; secrets; tests changed.
	in = labels.NewInputs([]string{"internal/x.go", "internal/x_test.go"}, true)
	f = labels.Facts(eff, in, labels.Run{
		Compat: &compatcheck.Run{Res: &compat.Result{API: []compat.APIChanges{{Spec: "api.yaml", Items: []compat.APIChange{{Level: 3}}}}}},
		Lint:   &linters.Run{Res: &lint.Result{Findings: []lint.Finding{{Tool: lint.ToolGitleaks}}}},
		TBD:    &pipeline.TBDStatus{SizeKnown: true, SizePartial: true, Added: 10},
	})
	want = map[string]labels.Tri{config.FactMigrations: labels.TriNo, config.FactAPIChanged: labels.TriYes, config.FactAPIBreaking: labels.TriYes,
		config.FactBigMR: labels.TriUnknown, config.FactLintSecret: labels.TriYes, config.FactNoTestsChanged: labels.TriNo}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("second: %s = %v, want %v", k, f[k], v)
		}
	}
	f = labels.Facts(eff, labels.NewInputs(nil, false), labels.Run{Compat: &compatcheck.Run{Res: &compat.Result{
		Issues: []compat.Issue{{Source: "oasdiff", Kind: compat.IssueFailed}}}}, Lint: &linters.Run{Res: &lint.Result{}}})
	for _, k := range []string{config.FactMigrations, config.FactAPIBreaking, config.FactBigMR, config.FactLintSecret, config.FactNoTestsChanged} {
		if f[k] != labels.TriUnknown {
			t.Errorf("unknown expected for %s: %v", k, f[k])
		}
	}
	if f[config.FactAPIChanged] != labels.TriNo {
		t.Error("api_changed comes from discovery")
	}
	// Compat off: the OpenAPI changelog.
	f = labels.Facts(eff, labels.NewInputs(nil, false), labels.Run{API: &compatcheck.APIRun{Specs: []compat.APIChanges{{Spec: "a.yaml", Items: []compat.APIChange{{Level: 1}}}}}})
	if f[config.FactAPIChanged] != labels.TriYes || f[config.FactAPIBreaking] != labels.TriNo {
		t.Errorf("summary-only facts %v", f)
	}
	for p, want := range map[string]bool{"a_test.go": true, "web/src/x.spec.ts": true, "tests/unit/a.py": true, "FooTest.php": true,
		"src/x.go": false, "vendor/a/b_test.go": true} {
		if labels.IsTestFile(p) != want {
			t.Errorf("isTestFile(%s)", p)
		}
	}
	if labels.IsCodeFile("vendor/a/b.go") || labels.IsCodeFile("README.md") || !labels.IsCodeFile("src/App.tsx") {
		t.Error("isCodeFile")
	}
}

// A label rule never sets the size-override label: that would lift
// review.max_mr_lines and big_mr.block by itself.
func TestLabelsNeverSetOverrideLabel(t *testing.T) {
	lh := reviewtest.NewLabelHarness(t)
	lh.GL.RepoConfig = "labels:\n  rules:\n    - name: Big-MR-OK\n      paths: [\"**/*.go\"]\n    - name: backend\n      paths: [\"**/*.go\"]\n"
	lh.Mr.Project = append(lh.Mr.Project, "big-mr-ok", "Big-MR-OK")
	lh.Review(t, reviewtest.AutoJob(), reviewtest.ReviewOut(t, nil, nil))
	if got := strings.Join(lh.Labels(), ","); got != "backend" {
		t.Fatalf("labels %s", got)
	}
	if got := labels.WithoutOverrideLabel([]string{"a", "BIG-mr-ok", "b"}, "big-mr-ok"); strings.Join(got, ",") != "a,b" {
		t.Fatalf("filter %v", got)
	}
	if got := labels.WithoutOverrideLabel([]string{"big-mr-ok"}, ""); len(got) != 1 {
		t.Fatalf("no override label configured: %v", got)
	}
}
