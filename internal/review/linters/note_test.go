package linters

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

// CI linter tool names used by the tests (lint.ci entries).
const (
	lintToolSemgrep = "ci:semgrep"
	lintToolRuff    = "ci:ruff"
	lintToolESLint  = "ci:eslint"
	lintToolPHPStan = "ci:phpstan"
)

const leakedValue = "AKIAZ7Q3XSUPERSECRET42"

var secretFinding = lint.Finding{Tool: lint.ToolGitleaks, Rule: "aws-access-token", Path: "cache.go", Line: 2,
	Severity: core.SeverityCritical, Message: "AWS access key", Commit: "0123456789ab", InDiff: true}

func TestLintSecretBodyHasNoValueFields(t *testing.T) {
	body := NoteBody("abc", secretFinding, true, false, false)
	for _, bad := range []string{"Secret", "Match", leakedValue} {
		if strings.Contains(body, bad) {
			t.Errorf("secret note contains %q:\n%s", bad, body)
		}
	}
}

func TestLintMarkerAndChatStrip(t *testing.T) {
	b := NoteBody("0123456789ab", lint.Finding{Tool: lintToolESLint, Rule: "no-eval", Path: "a.js", Line: 1, Message: "eval <b>can</b> be harmful", Severity: core.SeverityMinor}, false, false, false)
	id, tool, ok := marker.ParseLint(b)
	if !ok || id != "0123456789ab" || tool != lintToolESLint {
		t.Fatalf("%q %q %v", id, tool, ok)
	}
	if strings.Contains(b, "<b>") {
		t.Errorf("tool text must be escaped:\n%s", b)
	}
	if _, ok := marker.ParseFinding(b); ok {
		t.Error("lint note must not look like a finding")
	}
	if _, _, ok := marker.ParseCompat(b); ok {
		t.Error("lint note must not look like a compat thread")
	}
	if strings.Contains(marker.StripNoteMarkers(b), "reviewbot:lint") {
		t.Error("chat must strip the lint marker")
	}
}
