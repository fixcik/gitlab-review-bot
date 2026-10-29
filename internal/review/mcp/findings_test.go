package mcp

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func TestMergeFindings(t *testing.T) {
	sub := []output.Finding{
		{Path: "a.go", Line: 1, Severity: "minor", Title: "A"},
		{Path: "b.go", Line: 2, Severity: "critical", Title: "B"},
	}
	final := []output.Finding{
		{Path: "a.go", Line: 1, Severity: "major", Title: " a "}, // same path, line, title
		{Path: "a.go", Line: 5, Severity: "nit", Title: "A"},     // other line
		{Path: "c.go", Line: 3, Severity: "major", Title: "C"},
	}
	got := mergeFindings(sub, final, 0)
	var keys []string
	for _, f := range got {
		keys = append(keys, f.Path+":"+f.Title+":"+string(f.Severity))
	}
	if strings.Join(keys, " ") != "b.go:B:critical c.go:C:major a.go:A:minor a.go:A:nit" {
		t.Errorf("merged %v", keys)
	}
	if got := mergeFindings(sub, final, 2); len(got) != 2 || got[0].Title != "B" {
		t.Errorf("cap: %+v", got)
	}
	var m *Run
	if got := m.Merge(final); len(got) != 3 {
		t.Errorf("nil run must keep the final findings: %v", got)
	}
}
