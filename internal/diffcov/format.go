package diffcov

import (
	"fmt"
	"sort"
	"strings"
)

// Merge combines several reports (e.g. unit and integration jobs): a line
// is covered when any report covers it.
func Merge(reps ...*Report) *Report {
	out := &Report{Files: map[string]map[int]int{}}
	var formats []string
	for _, r := range reps {
		if r == nil {
			continue
		}
		formats = append(formats, string(r.Format))
		for f, lines := range r.Files {
			for ln, hits := range lines {
				out.add(f, ln, hits)
			}
		}
	}
	out.Format = Format(strings.Join(uniq(formats), "+"))
	return out
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Text renders a plain-text report for the agent (diffcov CLI).
// maxRanges caps the uncovered ranges listed per file (0 = all).
func Text(res Result, format Format, maxRanges int) string {
	var b strings.Builder
	if res.Total == 0 {
		fmt.Fprintf(&b, "Diff coverage: no executable changed lines found in the report (%s).\n", format)
	} else {
		fmt.Fprintf(&b, "Diff coverage: %.1f%% (%d/%d changed executable lines covered), report: %s\n",
			res.Percent(), res.Covered, res.Total, format)
	}
	files := append([]FileResult(nil), res.Files...)
	sort.SliceStable(files, func(i, j int) bool { // least covered first
		return files[i].Percent() < files[j].Percent()
	})
	for _, f := range files {
		fmt.Fprintf(&b, "%s  %.1f%%  %d/%d", f.Path, f.Percent(), f.Covered, f.Total)
		if len(f.Uncovered) > 0 {
			b.WriteString("  uncovered: " + RangesText(f.Uncovered, maxRanges))
		}
		b.WriteString("\n")
	}
	if len(res.NotInReport) > 0 {
		b.WriteString("Changed source files missing from the report (not run by tests, excluded, or path mismatch): " +
			strings.Join(res.NotInReport, ", ") + "\n")
	}
	return b.String()
}

// RangesText joins ranges ("3-6, 12"), listing at most limit (0 = all).
func RangesText(rs []Range, limit int) string {
	parts := make([]string, 0, len(rs))
	for i, r := range rs {
		if limit > 0 && i == limit {
			parts = append(parts, fmt.Sprintf("… +%d more", len(rs)-limit))
			break
		}
		parts = append(parts, r.String())
	}
	return strings.Join(parts, ", ")
}
