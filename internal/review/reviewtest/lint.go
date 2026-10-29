package reviewtest

import (
	"context"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

// CI linter tool names used by the tests (lint.ci entries).
const (
	LintToolSemgrep  = "ci:semgrep"
	LintToolRuff     = "ci:ruff"
	LintToolGolangci = "ci:golangci-lint"
	LintToolESLint   = "ci:eslint"
	LintToolPHPStan  = "ci:phpstan"
	LintToolPHPCS    = "ci:phpcs"
)

type FakeLint struct {
	mu    sync.Mutex
	Res   *lint.Result
	Err   error
	Calls []lint.Request
}

func (f *FakeLint) Check(_ context.Context, req lint.Request) (*lint.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, req)
	if f.Err != nil {
		return nil, f.Err
	}
	if f.Res == nil {
		return &lint.Result{}, nil
	}
	r := *f.Res
	r.Findings = append([]lint.Finding(nil), f.Res.Findings...)
	return &r, nil
}

func LintHarness(t *testing.T) (*Harness, *FakeLint) {
	h := New(t)
	fl := &FakeLint{}
	h.Deps.Lint = fl
	return h, fl
}

func Ran(tools ...string) []lint.ToolStatus {
	var out []lint.ToolStatus
	for _, t := range tools {
		out = append(out, lint.ToolStatus{Tool: t, State: lint.StateRan})
	}
	return out
}

var (
	SecretFinding = lint.Finding{Tool: lint.ToolGitleaks, Rule: "aws-access-token", Path: "cache.go", Line: 2,
		Severity: core.SeverityCritical, Message: "AWS access key", Commit: "0123456789ab", InDiff: true}
	// SemgrepFinding: a SAST report from CI whose lint.ci entry is blocking
	// (the severity gate decides).
	SemgrepFinding = lint.Finding{Tool: LintToolSemgrep, Rule: "go.lang.security.audit.dangerous-exec-command", Path: "cache.go", Line: 3,
		Severity: core.SeverityMajor, Message: "Detected non-static command inside Command", URL: "https://sg.run/ydO7", InDiff: true, MayBlock: true}
	RuffFinding = lint.Finding{Tool: LintToolRuff, Rule: "F401", Path: "cache.go", Line: 3,
		Severity: core.SeverityMinor, Message: "`os` imported but unused", URL: "https://docs.astral.sh/ruff/rules/unused-import", InDiff: true}
)

func LintThreads(h *Harness) []DraftNote {
	var out []DraftNote
	for _, d := range h.GL.Published {
		if _, _, ok := marker.ParseLint(d.Body); ok {
			out = append(out, d)
		}
	}
	return out
}
