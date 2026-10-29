package review

import (
	"errors"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/migrations"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

const DocsDiff = "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1,2 +1,3 @@\n # API\n+Новый абзац.\n-old\n+new\n"

func routeEff(t *testing.T) config.Effective {
	t.Helper()
	svc := config.Defaults()
	return config.Merge(&svc, nil)
}

func TestDecideRoute(t *testing.T) {
	eff := routeEff(t)
	off := eff
	off.Routing.Enabled = false
	big := strings.Repeat("+x\n", 70)
	for _, tc := range []struct {
		name   string
		eff    config.Effective
		in     routeInput
		route  string
		footer string
	}{
		{"docs only", eff, routeInput{files: []string{"README.md"}, diff: DocsDiff}, routeLight,
			"модель: haiku (низкий риск: только docs, 3 строки)"},
		{"docs and tests", eff, routeInput{files: []string{"docs/a.md", "pkg/a_test.go"}, diff: DocsDiff}, routeLight,
			"модель: haiku (низкий риск: только docs и тесты, 3 строки)"},
		{"code", eff, routeInput{files: []string{"README.md", "main.go"}, diff: DocsDiff}, routeNormal, "модель: sonnet"},
		{"too many lines", eff, routeInput{files: []string{"README.md"},
			diff: "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -0,0 +1,70 @@\n" + big}, routeNormal, "модель: sonnet"},
		{"sensitive path", eff, routeInput{files: []string{"docs/security/policy.md"}, diff: DocsDiff}, routeNormal,
			"модель: sonnet (риск: чувствительный путь)"},
		{"migrations", eff, routeInput{files: []string{"README.md"}, diff: DocsDiff, mg: &migcheck.Run{Res: &migrations.Result{}}}, routeNormal,
			"модель: sonnet (риск: миграции)"},
		{"api spec", eff, routeInput{files: []string{"docs/openapi.md"}, diff: DocsDiff,
			cr: &compatcheck.Run{Enabled: true, Res: &compat.Result{Specs: []compat.Spec{{Kind: "openapi", Path: "docs/openapi.yaml"}}}}},
			routeNormal, "модель: sonnet (риск: изменения API)"},
		{"compat failed", eff, routeInput{files: []string{"README.md"}, diff: DocsDiff, cr: &compatcheck.Run{Enabled: true, Err: errors.New("x")}},
			routeNormal, "модель: sonnet (риск: изменения API)"},
		{"api changelog", eff, routeInput{files: []string{"README.md"}, diff: DocsDiff, api: &compatcheck.APIRun{Specs: []compat.APIChanges{{Spec: "a.yaml"}}}},
			routeNormal, "модель: sonnet (риск: изменения API)"},
		{"secret", eff, routeInput{files: []string{"README.md"}, diff: DocsDiff,
			lr: &linters.Run{Res: &lint.Result{Findings: []lint.Finding{{Tool: lint.ToolGitleaks, Path: "README.md", Line: 2}}}}},
			routeNormal, "модель: sonnet (риск: секреты)"},
		{"sysmap contract", eff, routeInput{files: []string{"README.md"}, diff: DocsDiff,
			sm: &crossrepo.SysmapRun{Impact: &sysmap.Impact{Contracts: []sysmap.Touched{{}}}}}, routeNormal, "модель: sonnet (риск: контракты карты системы)"},
		{"routing off", off, routeInput{files: []string{"README.md"}, diff: DocsDiff}, routeNormal, ""},
		// A rename is judged by both paths: a file moved out of a
		// sensitive or code path is not low-risk.
		{"renamed from a sensitive path", eff, routeInput{files: []string{"docs/policy.md"},
			diff: "diff --git a/internal/auth/policy.md b/docs/policy.md\nsimilarity index 100%\nrename from internal/auth/policy.md\nrename to docs/policy.md\n" + DocsDiff},
			routeNormal, "модель: sonnet (риск: чувствительный путь)"},
		{"renamed from code", eff, routeInput{files: []string{"docs/main.md"},
			diff: "diff --git a/main.go b/docs/main.md\nsimilarity index 90%\nrename from main.go\nrename to docs/main.md\n--- a/main.go\n+++ b/docs/main.md\n@@ -1 +1 @@\n-a\n+b\n"},
			routeNormal, "модель: sonnet"},
		{"renamed quoted path", eff, routeInput{files: []string{"docs/ф.md"},
			diff: "diff --git \"a/auth/\\321\\204.md\" \"b/docs/\\321\\204.md\"\nrename from \"auth/\\321\\204.md\"\nrename to \"docs/\\321\\204.md\"\n" + DocsDiff},
			routeNormal, "модель: sonnet (риск: чувствительный путь)"},
		{"renamed within docs", eff, routeInput{files: []string{"docs/b.md"},
			diff: "diff --git a/docs/a.md b/docs/b.md\nsimilarity index 100%\nrename from docs/a.md\nrename to docs/b.md\n" + DocsDiff},
			routeLight, "модель: haiku (низкий риск: только docs, 3 строки)"},
		{"requirements", eff, routeInput{files: []string{"requirements/base.txt"}, diff: DocsDiff}, routeNormal, "модель: sonnet"},
		{"requirements in docs", eff, routeInput{files: []string{"docs/requirements.txt"}, diff: DocsDiff}, routeNormal,
			"модель: sonnet (риск: чувствительный путь)"},
	} {
		rt := decideRoute(tc.eff, "sonnet", tc.in)
		if rt.name != tc.route || rt.footer() != tc.footer {
			t.Errorf("%s: route %s footer %q, want %s %q (reasons %v)", tc.name, rt.name, rt.footer(), tc.route, tc.footer, rt.reasons)
		}
	}
}

// oldPaths reads the pre-image side of every file header of a diff.
func TestOldPaths(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n--- a/not-a-header.go\n+x\n" +
		"diff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1 @@\n+x\n" +
		"diff --git a/src.go b/dst.go\nsimilarity index 80%\ncopy from src.go\ncopy to dst.go\n" +
		"diff --git a/sp ace.go b/x.go\nrename from sp ace.go\nrename to x.go\n--- a/sp ace.go\t\n+++ b/x.go\t\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git \"a/bad\" b/bad\nrename from \"bad\\q\"\nrename to bad\n"
	got := strings.Join(oldPaths(diff), "|")
	if want := `a.go|src.go|sp ace.go|"bad\q"`; got != want {
		t.Fatalf("old paths %q, want %q", got, want)
	}
}

func codeDiff(n int) string {
	return "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -0,0 +1 @@\n" + strings.Repeat("+x\n", n)
}

// The heavy route: off without heavy_model; on, a range of at least
// heavy_min_lines changed lines or with a file under heavy_paths goes to
// the heavy model; the light route keeps its priority; routing.enabled
// (the light route) does not switch it off.
func TestDecideHeavyRoute(t *testing.T) {
	eff := routeEff(t)
	heavy := eff
	heavy.Routing.HeavyModel, heavy.Routing.HeavyMinLines, heavy.Routing.HeavyPaths = "opus", 300, []string{"**/migrations/**", "**/*.md"}
	noLight := heavy
	noLight.Routing.Enabled = false
	for _, tc := range []struct {
		name   string
		eff    config.Effective
		in     routeInput
		route  string
		footer string
		why    string
	}{
		{"off by default", eff, routeInput{files: []string{"main.go"}, diff: codeDiff(1356)}, routeNormal, "модель: sonnet", ""},
		{"below the threshold", heavy, routeInput{files: []string{"main.go"}, diff: codeDiff(299)}, routeNormal, "модель: sonnet", ""},
		{"at the threshold", heavy, routeInput{files: []string{"main.go"}, diff: codeDiff(300)}, routeHeavy,
			"модель: opus (тяжёлый: 300 строк ≥ 300)", "тяжёлый: 300 строк ≥ 300"},
		{"heavy path", heavy, routeInput{files: []string{"main.go", "db/migrations/001.sql"}, diff: codeDiff(2)}, routeHeavy,
			"модель: opus (тяжёлый: путь db/migrations/001.sql)", "тяжёлый: путь db/migrations/001.sql"},
		{"renamed from a heavy path", heavy, routeInput{files: []string{"db/old.sql"},
			diff: "diff --git a/db/migrations/1.sql b/db/old.sql\nrename from db/migrations/1.sql\nrename to db/old.sql\n" + codeDiff(1)}, routeHeavy,
			"модель: opus (тяжёлый: путь db/migrations/1.sql)", ""},
		{"light first", heavy, routeInput{files: []string{"README.md"}, diff: DocsDiff}, routeLight,
			"модель: haiku (низкий риск: только docs, 3 строки)", ""},
		{"without the light route", noLight, routeInput{files: []string{"main.go"}, diff: codeDiff(400)}, routeHeavy,
			"модель: opus (тяжёлый: 400 строк ≥ 300)", ""},
	} {
		rt := decideRoute(tc.eff, "sonnet", tc.in)
		if rt.name != tc.route || rt.footer() != tc.footer || (tc.why != "" && rt.why() != tc.why) {
			t.Errorf("%s: route %s footer %q why %q, want %s %q %q (reasons %v)", tc.name, rt.name, rt.footer(), rt.why(), tc.route, tc.footer, tc.why, rt.reasons)
		}
	}
}
