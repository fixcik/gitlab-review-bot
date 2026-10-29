// Package finding is the model of a review finding: its stable id, the
// severities, the normalization of findings from Claude's answers, and the
// first note of a finding thread (rendering and parsing). It is a leaf: it
// must not depend on internal/review or any of its feature packages.
package finding

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// ID is the stable id of a finding: first 12 hex chars of
// sha1(path + "\n" + lowercased trimmed title).
func ID(path, title string) string {
	sum := sha1.Sum([]byte(path + "\n" + strings.ToLower(strings.TrimSpace(title))))
	return hex.EncodeToString(sum[:])[:12]
}

// Rank orders severities: 0 is the most severe (critical), 3 a nit or
// anything unknown.
func Rank(s core.Severity) int {
	switch s {
	case core.SeverityCritical:
		return 0
	case core.SeverityMajor:
		return 1
	case core.SeverityMinor:
		return 2
	default:
		return 3
	}
}

// Badge is the severity as shown in notes (emoji and name).
func Badge(s core.Severity) string {
	switch s {
	case core.SeverityCritical:
		return "🔴 Critical"
	case core.SeverityMajor:
		return "🟠 Major"
	case core.SeverityMinor:
		return "🟡 Minor"
	default:
		return "⚪ Nit"
	}
}

// AllSeverities lists the severities from the most severe.
var AllSeverities = []core.Severity{core.SeverityCritical, core.SeverityMajor, core.SeverityMinor, core.SeverityNit}

// NormalizeReview cleans Claude's review answer in place: bot markers
// stripped from its texts, findings normalized (see Normalize) and capped
// at maxFindings (0 = no cap), entries without a path or id dropped.
func NormalizeReview(out *output.ReviewOutput, maxFindings int) {
	out.Summary = strings.TrimSpace(marker.StripBot(out.Summary))
	fs := out.Findings[:0]
	for _, f := range out.Findings {
		if f, ok := Normalize(f); ok {
			fs = append(fs, f)
		}
	}
	out.Findings = Cap(fs, maxFindings)

	ws := out.Walkthrough[:0]
	for _, w := range out.Walkthrough {
		w.Path = CleanPath(w.Path)
		w.Summary = strings.TrimSpace(marker.StripBot(w.Summary))
		if w.Path != "" {
			ws = append(ws, w)
		}
	}
	out.Walkthrough = ws

	rs := out.Resolved[:0]
	for _, r := range out.Resolved {
		r.ID = strings.ToLower(strings.TrimSpace(r.ID))
		r.Comment = strings.TrimSpace(marker.StripBot(r.Comment))
		if r.ID != "" {
			rs = append(rs, r)
		}
	}
	out.Resolved = rs
}

// Normalize cleans one finding of Claude's answer (or of
// submit_finding); ok=false drops it (no path or title). An unknown severity
// becomes minor.
func Normalize(f output.Finding) (output.Finding, bool) {
	f.Path = CleanPath(f.Path)
	f.Title = strings.TrimSpace(marker.StripBot(f.Title))
	f.Body = strings.TrimSpace(marker.StripBot(f.Body))
	if f.Path == "" || f.Title == "" {
		return f, false
	}
	switch f.Severity = core.Severity(strings.ToLower(strings.TrimSpace(string(f.Severity)))); f.Severity {
	case core.SeverityCritical, core.SeverityMajor, core.SeverityMinor, core.SeverityNit:
	default:
		f.Severity = core.SeverityMinor
	}
	if f.Line < 0 {
		f.Line = 0
	}
	if f.EndLine < f.Line {
		f.EndLine = f.Line
	}
	if strings.TrimSpace(f.Suggestion) == "" {
		f.Suggestion = ""
	}
	if u := f.DeployedUsage; u != nil {
		// Checked (and dropped where it does not apply) by
		// migcheck.Feature.Enforce; absolute or ".." paths are rejected there.
		c := output.UsageRef{Path: strings.TrimPrefix(strings.TrimSpace(u.Path), "./"), Line: u.Line, Name: strings.TrimSpace(u.Name)}
		f.DeployedUsage = &c
	}
	return f, true
}

// Cap orders findings by severity (stable) and keeps at most max
// (0 = no cap).
func Cap(fs []output.Finding, max int) []output.Finding {
	sort.SliceStable(fs, func(i, j int) bool { return Rank(fs[i].Severity) < Rank(fs[j].Severity) })
	if max > 0 && len(fs) > max {
		fs = fs[:max]
	}
	return fs
}

// CleanPath trims a repository path and drops a leading "./" and slashes.
func CleanPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	return strings.TrimLeft(p, "/")
}
