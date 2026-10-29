package linters

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// DuplicateOf: same file, the lint line within one line of the finding's
// range, and the finding names the rule (semgrep: last dotted part) or the
// tool; for secrets any secret wording.
func TestDuplicatesLintMut(t *testing.T) {
	sg := lint.Finding{Tool: lintToolSemgrep, Rule: "go.lang.security.audit.Dangerous-Exec", Path: "a.go", Line: 10}
	ruff := lint.Finding{Tool: lintToolRuff, Rule: "F401", Path: "a.go", Line: 10}
	leak := lint.Finding{Tool: lint.ToolGitleaks, Rule: "aws-access-token", Path: "a.go", Line: 10}
	f := func(line, end int, title string) output.Finding {
		return output.Finding{Path: "a.go", Line: line, EndLine: end, Title: title}
	}
	for _, tc := range []struct {
		name string
		f    output.Finding
		l    lint.Finding
		want bool
	}{
		{"same line, rule tail", f(10, 10, "dangerous-exec here"), sg, true},
		{"line before window", f(11, 11, "dangerous-exec"), sg, true},
		{"line after window", f(8, 9, "dangerous-exec"), sg, true},
		{"too far before", f(12, 12, "dangerous-exec"), sg, false},
		{"too far after", f(7, 8, "dangerous-exec"), sg, false},
		{"end before start uses line", f(11, 3, "dangerous-exec"), sg, true},
		{"end before start, far", f(12, 3, "dangerous-exec"), sg, false},
		{"an earlier dotted part does not count", f(10, 10, "audit problem"), sg, false},
		{"tool named", f(10, 10, "as Semgrep says"), sg, true},
		{"other file", output.Finding{Path: "b.go", Line: 10, Title: "dangerous-exec"}, sg, false},
		{"linter rule", f(10, 10, "f401: unused import"), ruff, true},
		{"linter unrelated", f(10, 10, "unused import"), ruff, false},
		{"other tools: the whole dotted id", f(10, 10, "wrong type here"), lint.Finding{Tool: lintToolPHPStan, Rule: "argument.type", Path: "a.go", Line: 10}, false},
		{"other tools: dotted id named", f(10, 10, "argument.type: wrong"), lint.Finding{Tool: lintToolPHPStan, Rule: "argument.type", Path: "a.go", Line: 10}, true},
		{"secret wording", f(10, 10, "Hardcoded API key"), leak, true},
		{"secret rule name alone is not enough", f(10, 10, "gitleaks: aws-access rule"), leak, false},
	} {
		if got := DuplicateOf(tc.f, []lint.Finding{tc.l}, nil) != nil; got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// Several lint results: a non-matching secret does not stop the search.
	if DuplicateOf(f(10, 10, "f401"), []lint.Finding{leak, ruff}, nil) == nil {
		t.Error("a secret result that does not match must not hide a later rule match")
	}
	lr := &Run{Res: &lint.Result{Findings: []lint.Finding{ruff}}}
	got := DropDuplicates([]output.Finding{f(10, 10, "F401 again"), f(10, 10, "deeper problem"), f(30, 30, "F401 elsewhere")}, lr, nil)
	if len(got) != 2 || got[0].Title != "deeper problem" || got[1].Title != "F401 elsewhere" {
		t.Errorf("DropDuplicates %+v", got)
	}
}
