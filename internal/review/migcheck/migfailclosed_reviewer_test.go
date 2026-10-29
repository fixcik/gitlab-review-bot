package migcheck_test

import (
	"context"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

type panickingCheckout struct{ core.Checkout }

func (panickingCheckout) ShowFile(context.Context, string, string) ([]byte, error) {
	panic("boom")
}

// A failure of the evidence check fails closed: unconfirmed blocking
// findings on migration files stop blocking, breaking-change items on
// migration files are already findings, other files are untouched.
func TestMigrationsEnforcementFailsClosed(t *testing.T) {
	h := reviewtest.New(t)
	m, co := evidenceRun(t, h)
	out := &output.ReviewOutput{
		Findings: []output.Finding{
			{Path: evMig, Line: 1, Severity: "critical", Title: "mig", DeployedUsage: &output.UsageRef{Path: "src/models/order.py", Line: 3, Name: "created_by"}},
			{Path: "src/app.py", Line: 1, Severity: "critical", Title: "app"},
		},
		Compatibility: &output.Compatibility{Breaking: []output.BreakingChange{{Path: evMig, Line: 2, Title: "compat on migration"}}},
	}
	h.Internals().Migrations.Enforce(context.Background(), discardLog(), panickingCheckout{co}, out, m, "English")
	got := map[string]core.Severity{}
	for _, f := range out.Findings {
		got[f.Title] = f.Severity
		if f.Title == "mig" && f.DeployedUsage != nil {
			t.Error("deployed_usage kept after a failed check")
		}
	}
	if got["mig"] != "major" || got["app"] != "critical" || got["compat on migration"] != "major" {
		t.Errorf("severities after panic: %v", got)
	}
	if len(out.Compatibility.Breaking) != 0 {
		t.Errorf("breaking items left on a migration: %+v", out.Compatibility.Breaking)
	}
}
