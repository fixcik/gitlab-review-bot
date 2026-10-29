package review_test

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestFormatMutNormalizeReview(t *testing.T) {
	out, err := review.ParseReviewOutput("```json\n"+reviewtest.MustJSON(t, output.ReviewOutput{
		Summary: " <!-- reviewbot:summary sha=abc -->Итог ",
		Findings: []output.Finding{
			{Path: "./a.go", Title: " First ", Severity: " CRITICAL ", Line: -4, EndLine: -9, Suggestion: " \n\t"},
			{Path: "/b.go", Title: "Second", Severity: "blocker", Line: 7, EndLine: 3},
			{Path: "c.go", Title: "<!-- reviewbot:blocking -->", Severity: "critical"},
			{Path: "c.go", Title: "Third", Severity: "minor", Line: 2, EndLine: 4, Suggestion: "x"},
			{Path: "d.go", Title: "Fourth", Severity: "nit", DeployedUsage: &output.UsageRef{Path: " ./svc/x.go ", Line: 3, Name: " F "}},
		},
		Walkthrough: []output.FileSummary{{Path: " ", Summary: "dropped"}, {Path: "./a.go", Summary: " s <!-- reviewbot:fixed --> "}},
		Resolved:    []output.ResolvedFinding{{ID: " ABCDEF012345 ", Comment: " ok <!-- reviewbot:fixed -->"}, {ID: "  "}},
	})+"\n```", 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary != "Итог" {
		t.Errorf("summary %q", out.Summary)
	}
	if len(out.Findings) != 4 {
		t.Fatalf("findings %+v", out.Findings)
	}
	// Sorted by severity, stable: critical, minor (unknown severity), minor, nit.
	f := out.Findings
	if f[0].Title != "First" || f[1].Title != "Second" || f[2].Title != "Third" || f[3].Title != "Fourth" {
		t.Fatalf("order %+v", f)
	}
	if f[0].Path != "a.go" || f[0].Severity != core.SeverityCritical || f[0].Line != 0 || f[0].EndLine != 0 || f[0].Suggestion != "" {
		t.Errorf("first %+v", f[0])
	}
	if f[1].Path != "b.go" || f[1].Severity != core.SeverityMinor || f[1].Line != 7 || f[1].EndLine != 7 {
		t.Errorf("second %+v", f[1])
	}
	if f[2].Line != 2 || f[2].EndLine != 4 || f[2].Suggestion != "x" {
		t.Errorf("third %+v", f[2])
	}
	if u := f[3].DeployedUsage; u == nil || u.Path != "svc/x.go" || u.Name != "F" || u.Line != 3 {
		t.Errorf("deployed usage %+v", u)
	}
	if len(out.Walkthrough) != 1 || out.Walkthrough[0].Path != "a.go" || out.Walkthrough[0].Summary != "s" {
		t.Errorf("walkthrough %+v", out.Walkthrough)
	}
	if len(out.Resolved) != 1 || out.Resolved[0].ID != "abcdef012345" || out.Resolved[0].Comment != "ok" {
		t.Errorf("resolved %+v", out.Resolved)
	}
}

func TestFormatMutMaxFindings(t *testing.T) {
	var fs []output.Finding
	for _, s := range []string{"nit", "minor", "major", "critical"} {
		fs = append(fs, output.Finding{Path: "a.go", Title: s, Severity: core.Severity(s)})
	}
	js := reviewtest.MustJSON(t, output.ReviewOutput{Findings: fs})
	if out, _ := review.ParseReviewOutput(js, 0); len(out.Findings) != 4 {
		t.Errorf("0 = no limit: %d", len(out.Findings))
	}
	if out, _ := review.ParseReviewOutput(js, 4); len(out.Findings) != 4 {
		t.Errorf("limit equal to count: %d", len(out.Findings))
	}
	out, _ := review.ParseReviewOutput(js, 1)
	if len(out.Findings) != 1 || out.Findings[0].Severity != core.SeverityCritical {
		t.Errorf("clamp keeps the most severe: %+v", out.Findings)
	}
}
