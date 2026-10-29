package config

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRepoCollectsEveryProblem(t *testing.T) {
	y := "languag: ru\n" + // 1 unknown key
		"lint:\n" + // 2
		"  disabel: [x]\n" + // 3 unknown nested key
		"  disable: [nope]\n" + // 4 invalid value (reported on the section line)
		"tbd:\n" + // 5
		"  max_lines: -1\n" + // 6
		"max_files: abc\n" + // 7 wrong type
		"limits: [1]\n" + // 8 wrong type
		"path_instructions:\n" + // 9
		"  - path: \"a/**\"\n" + // 10
		"    instruction: x\n" // 11 unknown key in a list item
	r, issues := ValidateRepo([]byte(y))
	if r != nil {
		t.Fatal("an invalid config must not be returned")
	}
	want := []string{
		`line 1: languag: unknown key (did you mean "language"?)`,
		`line 2: lint.disable: unknown tool "nope"`,
		`line 3: lint.disabel: unknown key (did you mean "lint.disable"?)`,
		`line 5: tbd.max_lines must be >= 0`,
		`line 7: max_files: cannot unmarshal`,
		`line 8: limits:`,
		`line 9: path_instructions[0] (a/**): instructions are required`,
		`line 11: path_instructions[0].instruction: unknown key (did you mean "path_instructions[0].instructions"?)`,
	}
	if len(issues) != len(want) {
		t.Fatalf("got %d issues, want %d:\n%s", len(issues), len(want), issues.Error())
	}
	for i, w := range want {
		if !strings.HasPrefix(issues[i].String(), w) {
			t.Errorf("issue %d = %q, want prefix %q", i, issues[i].String(), w)
		}
	}
	// ParseRepo returns the same list as its error.
	_, err := ParseRepo([]byte(y))
	var re RepoErrors
	if !errors.As(err, &re) || len(re) != len(want) || !strings.HasPrefix(err.Error(), RepoConfigPath+": line 1: ") {
		t.Fatalf("ParseRepo error %v", err)
	}
	if got := RepoIssuesOf(err); len(got) != len(want) {
		t.Fatalf("RepoIssuesOf: %d", len(got))
	}
	if got := RepoIssuesOf(errors.New("x")); len(got) != 1 || got[0].Message != "x" {
		t.Fatalf("RepoIssuesOf(other) = %v", got)
	}
	if RepoIssuesOf(nil) != nil {
		t.Fatal("RepoIssuesOf(nil)")
	}
}

func TestValidateRepoUnknownKeysRejected(t *testing.T) {
	for y, want := range map[string]string{
		"auto_reviw: false\n":                    `auto_reviw: unknown key (did you mean "auto_review"?)`,
		"compat:\n  enabeld: false\n":            `compat.enabeld: unknown key (did you mean "compat.enabled"?)`,
		"context:\n  related:\n    - rep: a/b\n": `context.related[0].rep: unknown key (did you mean "context.related[0].repo"?)`,
		"totally_new: 1\n":                       `totally_new: unknown key`,
		"defaults: &d\n  enabled: true\n":        `defaults: unknown key`,
	} {
		_, issues := ValidateRepo([]byte(y))
		if len(issues) == 0 || !strings.Contains(issues.Error(), want) {
			t.Errorf("%q: %v, want %q", y, issues, want)
		}
	}
}

func TestValidateRepoShapes(t *testing.T) {
	for _, ok := range []string{"", "  \n", "# yaml-language-server: $schema=x\n", "~\n", "enabled: false\n",
		"memory: {enabled: false}\nlint:\n"} {
		if r, issues := ValidateRepo([]byte(ok)); r == nil || len(issues) > 0 {
			t.Errorf("%q: %v", ok, issues)
		}
	}
	for y, want := range map[string]string{
		"a: [\n":                                  "line 1: ",
		"- a\n- b\n":                              "must be a mapping",
		"max_files: 1\nmax_files: 2\n":            "line 2: ",
		"enabled: true\n---\nx: 1\n":              "only one YAML document",
		strings.Repeat("#", MaxRepoConfigBytes+1): "larger than",
	} {
		_, issues := ValidateRepo([]byte(y))
		if len(issues) != 1 || !strings.Contains(issues[0].String(), want) {
			t.Errorf("%.40q: %v, want %q", y, issues, want)
		}
	}
}

func TestRequireRepoConfigDefault(t *testing.T) {
	if !Defaults().Review.RequireRepoConfig {
		t.Fatal("review.require_repo_config must default to true")
	}
}

// TestValidateRepoKnowsCurrentKeys: every section of the repo config that
// exists today is accepted with real values (a key ValidateRepo did not know
// would stop the reviews of the repository).
func TestValidateRepoKnowsCurrentKeys(t *testing.T) {
	body := `# yaml-language-server: $schema=https://example.invalid/reviewbot.schema.json
enabled: true
auto_review: true
language: en
ignore: ["gen/**"]
instructions: "be brief"
max_files: 50
limits: {user_reruns_per_hour: 2, user_chat_per_hour: 5, mr_rerun_cooldown: 30m}
jira: {project_keys: [PAY]}
memory: {enabled: false}
learn: {enabled: false}
context: {enabled: true, exclude: ["legacy/**"]}
compat: {enabled: true, openapi: ["api/*.yaml"], proto: ["proto/**"], graphql: ["*.graphql"]}
diagram: false
path_instructions:
  - {path: "db/**", instructions: "check locks"}
lint:
  enabled: true
  disable: [gitleaks]
  ci:
    - {job: phpstan, artifact: phpstan.json, format: codeclimate, severity: minor, blocking: true}
    - {job: ruff, report: codequality, tool: ruff}
todo: {enabled: true, keywords: [TODO, FIXME], severity: minor, blocking: false, ignore: ["vendor/**"]}
skills: {enabled: true, disable: [go-errors]}
tbd: {enabled: true, max_lines: 800, max_age: 48h}
coverage: {job: tests, path: coverage.xml}
migrations: {enabled: true, paths: ["db/**/*.sql"], dialect: postgres}
sysmap: {exclude: ["testdata/**"]}
mcp: {enabled: false}
problems: {summary: false}
reactions: {enabled: false}
labels:
  enabled: true
  rules:
    - {name: backend, paths: ["api/**"]}
    - {name: db, when_fact: migrations}
verify: {enabled: false}
max_mr_lines: 1500
big_mr: {block: true}
`
	r, issues := ValidateRepo([]byte(body))
	if len(issues) > 0 {
		t.Fatalf("issues: %v", issues)
	}
	if r.MaxMRLines != 1500 || r.BigMR == nil || r.BigMR.Block == nil || !*r.BigMR.Block || len(r.Lint.CI) != 2 || r.Todo == nil ||
		r.Reactions == nil || r.Verify == nil || r.Problems == nil || r.MCP == nil || r.Labels == nil {
		t.Fatalf("parsed: %+v", r)
	}
	// Values of the newer sections are checked too, all at once.
	_, issues = ValidateRepo([]byte("max_mr_lines: -1\ntodo:\n  severity: loud\nlint:\n  ci:\n    - {job: x}\n"))
	got := issues.Error()
	for _, want := range []string{"line 1: max_mr_lines must be >= 0", "line 2: todo.severity", "line 4: lint.ci[0]"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in %s", want, got)
		}
	}
}
