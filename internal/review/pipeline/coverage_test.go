package pipeline

import (
	"fmt"
	"github.com/fixcik/gitlab-review-bot/internal/diffcov"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"strings"
	"testing"
)

func TestCoverageHelpers(t *testing.T) {
	if !cijob.IsParallelOf("tests 3/10", "tests") || cijob.IsParallelOf("tests-slow", "tests") || cijob.IsParallelOf("tests a/b", "tests") {
		t.Error("isParallelOf")
	}
	if fmtPercent(50) != "50%" || fmtPercent(66.7) != "66.7%" {
		t.Error("fmtPercent")
	}
	body := "intro\n\n" + marker.TBDStart + "\nX\n" + marker.TBDEnd + "\n\n" + summary.CompatSectionTitle
	got := replaceCoverage(body, marker.CoverageStart+"\nC\n"+marker.CoverageEnd)
	if !strings.Contains(got, marker.TBDEnd+"\n\n"+marker.CoverageStart+"\nC\n"+marker.CoverageEnd+"\n\n"+summary.CompatSectionTitle) {
		t.Errorf("insert after tbd:\n%s", got)
	}
	if again := replaceCoverage(got, marker.CoverageStart+"\nD\n"+marker.CoverageEnd); strings.Contains(again, "\nC\n") || !strings.Contains(again, "\nD\n") {
		t.Errorf("replace:\n%s", again)
	}
}

// The prompt section (MCP off) lists the uncovered changed lines per file,
// least covered first, capped in files and ranges; files without uncovered
// lines are not listed.
func TestCoverageForClaudeCaps(t *testing.T) {
	if CoverageForClaude(nil) != "" || CoverageForClaude(&CoverageRun{State: covWaiting}) != "" {
		t.Fatal("unknown coverage must give no section")
	}
	var files []diffcov.FileResult
	for i := 0; i < covPromptFiles+3; i++ {
		var unc []diffcov.Range
		for l := 1; l <= 40; l += 2 { // 20 separate ranges
			unc = append(unc, diffcov.Range{From: l, To: l})
		}
		files = append(files, diffcov.FileResult{Path: fmt.Sprintf("f%02d.go", i), Covered: i, Total: 40, Uncovered: unc})
	}
	files = append(files, diffcov.FileResult{Path: "full.go", Covered: 5, Total: 5})
	s := CoverageForClaude(&CoverageRun{State: CoverageOK, Res: diffcov.Result{Covered: 10, Total: 100, Files: files}})
	if strings.Count(s, "\n- f") != covPromptFiles || !strings.Contains(s, "- and 3 more file(s) with uncovered lines.") {
		t.Errorf("file cap:\n%s", s)
	}
	if strings.Contains(s, "full.go") || !strings.Contains(s, "- f00.go: 0 of 40 covered") {
		t.Errorf("order or covered file:\n%s", s)
	}
	if strings.Contains(s, "diffcov") || !strings.Contains(s, "uncovered changed lines 1, 3, 5, 7, 9, 11, 13, 15, 17, 19, … +10 more\n") {
		t.Errorf("ranges cap:\n%s", s)
	}
	if s := CoverageForClaude(&CoverageRun{State: CoverageOK}); !strings.Contains(s, "no executable lines among the changed lines") {
		t.Errorf("empty: %s", s)
	}
}
