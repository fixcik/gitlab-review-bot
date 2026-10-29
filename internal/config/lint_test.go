package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestLintDefaults(t *testing.T) {
	c := Defaults()
	l := c.Review.Lint
	if !l.Enabled || l.Timeout.D() != 3*time.Minute || strings.Join(l.Tools, ",") != "gitleaks" || l.AllowDisableSecrets {
		t.Fatalf("%+v", l)
	}
	if errs := l.validate(); len(errs) != 0 {
		t.Fatalf("defaults must validate: %v", errs)
	}
	e := Merge(&c, nil)
	if !e.Lint.Enabled || strings.Join(e.Lint.Tools, ",") != "gitleaks" || e.Lint.SecretsKept || len(e.Lint.CI) != 0 {
		t.Fatalf("%+v", e.Lint)
	}
}

func TestLintRepoDisableKeepsSecrets(t *testing.T) {
	c := Defaults()
	r, err := ParseRepo([]byte("lint:\n  disable: [\" \", Gitleaks, \"\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.Lint.Disable, ",") != "gitleaks" {
		t.Errorf("disable %v", r.Lint.Disable)
	}
	e := Merge(&c, r)
	if got := strings.Join(e.Lint.Tools, ","); got != "gitleaks" || !e.Lint.SecretsKept {
		t.Fatalf("gitleaks must stay on: %s %+v", got, e.Lint)
	}
	c.Review.Lint.AllowDisableSecrets = true
	e = Merge(&c, r)
	if len(e.Lint.Tools) != 0 || e.Lint.SecretsKept || e.Lint.Enabled {
		t.Fatalf("allowed: %+v", e.Lint)
	}
}

func TestLintRepoOff(t *testing.T) {
	c := Defaults()
	r, err := ParseRepo([]byte("lint:\n  enabled: false\n  ci:\n    - job: phpstan\n      artifact: phpstan.json\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&c, r)
	if got := strings.Join(e.Lint.Tools, ","); got != "gitleaks" || !e.Lint.Enabled || !e.Lint.SecretsKept || len(e.Lint.CI) != 0 {
		t.Fatalf("lint off keeps gitleaks only, no CI linters: %+v", e.Lint)
	}
	c.Review.Lint.AllowDisableSecrets = true
	if e := Merge(&c, r); e.Lint.Enabled || len(e.Lint.Tools) != 0 {
		t.Fatalf("%+v", e.Lint)
	}
	c.Review.Lint.Enabled = false
	if e := Merge(&c, nil); e.Lint.Enabled {
		t.Fatal("service switch")
	}
}

func TestLintCIConfig(t *testing.T) {
	svc := Defaults()
	r, err := ParseRepo([]byte(`lint:
  ci:
    - job: "phpstan"
      artifact: "./build/phpstan.json"
      format: CodeClimate
      blocking: true
      severity: Major
    - job: "lint:ruff *"
      report: CodeQuality
    - job: semgrep
      artifact: gl-sast.sarif
      tool: SAST
`))
	if err != nil {
		t.Fatal(err)
	}
	l := Merge(&svc, r).Lint
	if len(l.CI) != 3 {
		t.Fatalf("ci %+v", l.CI)
	}
	a, b, c := l.CI[0], l.CI[1], l.CI[2]
	if a.Tool != "phpstan" || a.Artifact != "build/phpstan.json" || a.Format != "codeclimate" || !a.Blocking || a.Severity != "major" {
		t.Errorf("first %+v", a)
	}
	if b.Tool != "lint-ruff" || b.Report != CIReportCodeQuality || b.Format != CIFormatAuto || b.Artifact != "" {
		t.Errorf("second (tool from a glob job name) %+v", b)
	}
	if c.Tool != "sast" {
		t.Errorf("third %+v", c)
	}
	svc.Review.Lint.Tools = nil
	if !Merge(&svc, r).Lint.Enabled {
		t.Error("CI linters alone enable the lint step")
	}
	svc.Review.Lint.Enabled = false
	if l := Merge(&svc, r).Lint; l.Enabled || len(l.CI) != 0 {
		t.Error("the service switch wins")
	}
}

func TestLintCIValidation(t *testing.T) {
	for name, bad := range map[string]string{
		"no job":               "lint:\n  ci:\n    - artifact: a.json\n",
		"bad glob":             "lint:\n  ci:\n    - job: \"a[\"\n      artifact: a.json\n",
		"neither":              "lint:\n  ci:\n    - job: a\n",
		"both":                 "lint:\n  ci:\n    - job: a\n      artifact: a.json\n      report: codequality\n",
		"unknown report":       "lint:\n  ci:\n    - job: a\n      report: sast\n",
		"codequality as sarif": "lint:\n  ci:\n    - job: a\n      report: codequality\n      format: sarif\n",
		"escaping artifact":    "lint:\n  ci:\n    - job: a\n      artifact: ../x.json\n",
		"absolute artifact":    "lint:\n  ci:\n    - job: a\n      artifact: /x.json\n",
		"unknown format":       "lint:\n  ci:\n    - job: a\n      artifact: a.json\n      format: junit\n",
		"bad tool":             "lint:\n  ci:\n    - job: a\n      artifact: a.json\n      tool: \"a b\"\n",
		"tool from glob only":  "lint:\n  ci:\n    - job: \"*\"\n      artifact: a.json\n",
		"duplicate tool":       "lint:\n  ci:\n    - job: a\n      artifact: a.json\n    - job: b\n      artifact: b.json\n      tool: a\n",
		"bad severity":         "lint:\n  ci:\n    - job: a\n      artifact: a.json\n      severity: blocker\n",
	} {
		if _, err := ParseRepo([]byte(bad)); err == nil {
			t.Errorf("%s: must fail", name)
		}
	}
	var b strings.Builder
	b.WriteString("lint:\n  ci:\n")
	for i := 0; i <= MaxCILints; i++ {
		fmt.Fprintf(&b, "    - job: j%d\n      artifact: r.json\n", i)
	}
	if _, err := ParseRepo([]byte(b.String())); err == nil {
		t.Errorf("%d entries must fail", MaxCILints+1)
	}
}

// The bot no longer runs linters: every key of the old setup is a clear
// configuration error pointing at lint.ci.
func TestLintRemovedKeys(t *testing.T) {
	for _, bad := range []string{
		"lint:\n  custom:\n    - name: arch\n      command: go-arch-lint check\n",
		"lint:\n  semgrep_configs: [\"tools/semgrep/*.yml\"]\n",
		"lint:\n  disable: [eslint]\n",
		"lint:\n  disable: [PHPStan]\n",
	} {
		_, err := ParseRepo([]byte(bad))
		if err == nil || !strings.Contains(err.Error(), "linters now come from CI reports") || !strings.Contains(err.Error(), "lint.ci") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := ParseRepo([]byte("lint:\n  disable: [pylint]\n")); err == nil || strings.Contains(err.Error(), "CI reports") {
		t.Errorf("an unknown (never supported) tool is just unknown: %v", err)
	}

	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	for key, cfg := range map[string]string{
		"review.lint.semgrep_rules":      "    semgrep_rules: /opt/semgrep-rules\n",
		"review.lint.allow_custom":       "    allow_custom: true\n",
		`review.lint.tools: "semgrep"`:   "    tools: [gitleaks, semgrep]\n",
		`severity_overrides: "ruff"`:     "    severity_overrides: {ruff: nit}\n",
		`severity_overrides: "custom:x"`: "    severity_overrides: {\"custom:x\": nit}\n",
	} {
		_, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  groups: [g]\nreview:\n  lint:\n"+cfg))
		if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), LintRemovedHint) {
			t.Errorf("%s: %v", key, err)
		}
	}
	if _, err := LoadService(writeConfig(t, "gitlab:\n  url: https://g\n  groups: [g]\nreview:\n  lint:\n    tools: [gitleaks]\n")); err != nil {
		t.Errorf("gitleaks alone is fine: %v", err)
	}
}

func TestLintOverridesAndValidation(t *testing.T) {
	c := Defaults()
	c.Review.Lint.SeverityOverrides = map[string]string{"CI:PHPStan": "nit", "ci:semgrep:go.lang.X": "MAJOR", "gitleaks": "nit"}
	if errs := c.Review.Lint.validate(); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	e := Merge(&c, nil)
	if e.Lint.SeverityOverrides["ci:phpstan"] != core.SeverityNit || e.Lint.SeverityOverrides["ci:semgrep:go.lang.X"] != core.SeverityMajor {
		t.Fatalf("%v", e.Lint.SeverityOverrides)
	}
	c.Review.Lint.Tools = []string{"gitleaks", "pylint"}
	c.Review.Lint.SeverityOverrides = map[string]string{"ci:x": "blocker", "foo:bar": "nit", "ci": "nit", "ci:A B": "nit"}
	errs := strings.Join(c.Review.Lint.validate(), "; ")
	for _, s := range []string{`unknown tool "pylint"`, `severity_overrides["ci:x"]`, `unknown tool in "foo:bar"`, `"ci" must be`, `"ci:A B" must be`} {
		if !strings.Contains(errs, s) {
			t.Errorf("missing %q in %s", s, errs)
		}
	}
	if _, err := ParseRepo([]byte("lint:\n  disable: [pylint]\n")); err == nil {
		t.Error("unknown tool in disable must fail")
	}
}
