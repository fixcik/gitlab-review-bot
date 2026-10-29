package linters_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// ResultBlocking: secrets always block; CI linters marked blocking follow the
// severity gate; other CI linters never block; an in-process tool that is
// not a linter (a future scanner) follows the severity gate.
func TestLintBlockingMatrixMut(t *testing.T) {
	ci := lint.CIPrefix + "arch"
	for _, tc := range []struct {
		min  string
		f    lint.Finding
		want bool
	}{
		{"critical", lint.Finding{Tool: lint.ToolGitleaks, Severity: core.SeverityNit}, true},
		{"critical", lint.Finding{Tool: reviewtest.LintToolSemgrep, Severity: core.SeverityMajor, MayBlock: true}, false},
		{"critical", lint.Finding{Tool: reviewtest.LintToolSemgrep, Severity: core.SeverityCritical, MayBlock: true}, true},
		{"major", lint.Finding{Tool: reviewtest.LintToolSemgrep, Severity: core.SeverityMajor, MayBlock: true}, true},
		{"major", lint.Finding{Tool: reviewtest.LintToolRuff, Severity: core.SeverityCritical}, false},
		{"major", lint.Finding{Tool: ci, Severity: core.SeverityCritical}, false},
		{"major", lint.Finding{Tool: ci, Severity: core.SeverityMajor, MayBlock: true}, true},
		{"major", lint.Finding{Tool: ci, Severity: core.SeverityMinor, MayBlock: true}, false},
		{"critical", lint.Finding{Tool: ci, Severity: core.SeverityMajor, MayBlock: true}, false},
		{"major", lint.Finding{Tool: "scanner", Severity: core.SeverityMajor}, true},
	} {
		h := reviewtest.New(t)
		h.Cfg.Review.BlockMinSeverity = tc.min
		if got := h.Internals().Lint.ResultBlocking(tc.f); got != tc.want {
			t.Errorf("min %s, %s/%s may_block=%v: blocking %v, want %v", tc.min, tc.f.Tool, tc.f.Severity, tc.f.MayBlock, got, tc.want)
		}
	}
}

// ResolveNonBlocking resolves exactly the just-published non-blocking
// lint threads it is given, wherever they are in the discussion list.
func TestResolveNonBlockingLintMut(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.WithModerator(h)
	other := withSeverity(reviewtest.RuffFinding, core.SeverityMinor)
	other.Rule = "E501"
	sg := withSeverity(reviewtest.SemgrepFinding, core.SeverityCritical)
	h.GL.Discussions = []core.Discussion{
		{ID: "d-other", Notes: []core.Note{reviewtest.BotNote(1, linters.NoteBody(other.ID(), other, false, true, false), nil)}},
		{ID: "d-plain", Notes: []core.Note{reviewtest.BotNote(2, "no marker", nil)}},
		{ID: "d-blocking", Notes: []core.Note{reviewtest.BotNote(3, linters.NoteBody(sg.ID(), sg, true, false, false), nil)}},
		{ID: "d-done", Resolved: true, Notes: []core.Note{reviewtest.BotNote(4, linters.NoteBody(reviewtest.RuffFinding.ID(), reviewtest.RuffFinding, false, true, false), nil)}},
		{ID: "d-ruff", Notes: []core.Note{reviewtest.BotNote(5, linters.NoteBody(reviewtest.RuffFinding.ID(), reviewtest.RuffFinding, false, true, false), nil)}},
	}
	h.Internals().Lint.ResolveNonBlocking(context.Background(), reviewtest.AutoJob(), map[string]bool{reviewtest.RuffFinding.ID(): true})
	if got := strings.Join(m.Resolved, ","); got != "d-ruff" {
		t.Errorf("resolved %q", got)
	}
	// A blocking thread is never resolved, even when asked to.
	m.Resolved = nil
	h.Internals().Lint.ResolveNonBlocking(context.Background(), reviewtest.AutoJob(), map[string]bool{sg.ID(): true})
	if len(m.Resolved) != 0 {
		t.Errorf("resolved %v", m.Resolved)
	}
}

// A thread of a tool the repository turned off is never closed as fixed,
// even if a result claims that tool ran.
func TestResolveFixedLintToolOffMut(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.WithModerator(h)
	lr := &linters.Run{Cfg: config.Lint{Tools: []string{lint.ToolGitleaks}}, Res: &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks, reviewtest.LintToolRuff)},
		Threads: map[string]linters.Thread{
			"r": {DiscussionID: "d-ruff", Tool: reviewtest.LintToolRuff, Open: true},
			"g": {DiscussionID: "d-leak", Tool: lint.ToolGitleaks, Blocking: true, Open: true},
		}, Order: []string{"r", "g"}}
	h.Internals().Lint.ResolveFixed(context.Background(), reviewtest.AutoJob(), lr)
	if got := strings.Join(m.Resolved, ","); got != "d-leak" || lr.Closed != 1 || len(lr.Remaining) != 1 || lr.Remaining[0].Tool != reviewtest.LintToolRuff {
		t.Errorf("resolved %q closed %d remaining %+v", got, lr.Closed, lr.Remaining)
	}
	if lr.BlockingOpen() != 0 {
		t.Errorf("blocking open %d", lr.BlockingOpen())
	}
}
