package reviewtest

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
)

// RichHarness is a review with every analyzer that feeds the prompt: API
// changelog (compat), linters, the system map, migrations and coverage.
func RichHarness(t *testing.T) (*Harness, *FakeLint) {
	t.Helper()
	h := MigHarness(t)
	h.Repos.Dir = t.TempDir()
	h.Compat.Res = &compat.Result{Changes: []compat.Change{OasdiffChange}, API: []compat.APIChanges{ApiOrders}}
	fl := &FakeLint{Res: &lint.Result{Findings: []lint.Finding{SemgrepFinding, RuffFinding}, Tools: Ran(LintToolSemgrep, LintToolRuff)}}
	h.Deps.Lint = fl
	h.Cfg.Review.Lint.Enabled = true
	WithSysmap(h, &FakeSysmap{Imp: OrdersImpact()})
	c := CoverageHarness(t) // CI coverage report of cache.go
	h.Repos.Diff += "diff --git a/cache.go b/cache.go\n--- a/cache.go\n+++ b/cache.go\n@@ -1,3 +1,5 @@\n a\n+b\n+c\n d\n e\n"
	h.Repos.Files = append(h.Repos.Files, "cache.go")
	h.GL.RepoConfig, h.GL.MR.Pipeline, h.GL.Jobs, h.GL.Artifacts = c.GL.RepoConfig, c.GL.MR.Pipeline, c.GL.Jobs, c.GL.Artifacts
	return h, fl
}
