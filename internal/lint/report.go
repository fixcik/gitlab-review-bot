package lint

// Linter reports produced by the repository's own CI (.reviewbot.yaml
// lint.ci): GitLab Code Quality / CodeClimate JSON, SARIF 2.1.0 and
// Checkstyle XML. The bot does not run linters: the review package
// downloads the reports from the MR head pipeline (a job artifact or the
// job's `codequality` report), and this file parses them and keeps the
// results on the lines the MR adds. The tool name is CIPrefix + the entry's
// name ("ci:phpstan").

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// CIPrefix prefixes the tool name of CI linters ("ci:phpstan").
const CIPrefix = "ci:"

// IsCI reports whether tool is a linter whose results come from a CI report.
func IsCI(tool string) bool { return strings.HasPrefix(tool, CIPrefix) }

// ciName is the lint.ci entry name of a CI tool: the rule of results that
// name none.
func ciName(tool string) string { return strings.TrimPrefix(tool, CIPrefix) }

// Report formats (lint.ci[].format).
const (
	FormatAuto        = "auto"
	FormatCodeClimate = "codeclimate"
	FormatSARIF       = "sarif"
	FormatCheckstyle  = "checkstyle"
)

// MaxReportBytes caps one CI report (downloaded and, for gzip, unpacked).
const MaxReportBytes = 20 << 20

// ParseReport parses a CodeClimate / GitLab Code Quality JSON, SARIF 2.1.0
// or Checkstyle XML report (format "" or FormatAuto = detect). A gzip
// report is unpacked first. Paths are returned as written in the report.
func ParseReport(data []byte, format, tool string) ([]Finding, error) {
	if len(data) > MaxReportBytes {
		return nil, errors.New("report too large")
	}
	if bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		b, err := io.ReadAll(io.LimitReader(zr, MaxReportBytes+1))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		if len(b) > MaxReportBytes {
			return nil, errors.New("report too large")
		}
		data = b
	}
	t := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if len(t) == 0 {
		return nil, errors.New("empty report")
	}
	switch format {
	case "", FormatAuto:
		switch t[0] {
		case '[':
			return parseCodeClimate(t, tool)
		case '{':
			if looksLikeSARIF(t) {
				return parseSARIF(t, tool)
			}
			return parseCodeClimate(t, tool) // NUL/newline-separated issues
		case '<':
			return parseCheckstyle(t, tool)
		}
		return nil, errors.New("unknown report format")
	case FormatCodeClimate:
		return parseCodeClimate(t, tool)
	case FormatSARIF:
		return parseSARIF(t, tool)
	case FormatCheckstyle:
		return parseCheckstyle(t, tool)
	}
	return nil, fmt.Errorf("unknown format %q", format)
}

func looksLikeSARIF(t []byte) bool {
	var probe struct {
		Runs json.RawMessage `json:"runs"`
	}
	return json.Unmarshal(t, &probe) == nil && len(probe.Runs) > 0
}

// ---------------------------------------------------------------------------
// CodeClimate / GitLab Code Quality
// ---------------------------------------------------------------------------

// codeClimateIssue is one issue of the CodeClimate spec
// (github.com/codeclimate/platform/blob/master/spec/analyzers/SPEC.md) as
// GitLab Code Quality reads it: description, check_name, fingerprint,
// severity (info|minor|major|critical|blocker), location.path and
// location.lines.begin or location.positions.begin.line.
type codeClimateIssue struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	CheckName   string `json:"check_name"`
	Fingerprint string `json:"fingerprint"`
	Severity    string `json:"severity"`
	Location    struct {
		Path  string `json:"path"`
		Lines *struct {
			Begin int `json:"begin"`
			End   int `json:"end"`
		} `json:"lines"`
		Positions *struct {
			Begin struct {
				Line int `json:"line"`
			} `json:"begin"`
			End struct {
				Line int `json:"line"`
			} `json:"end"`
		} `json:"positions"`
	} `json:"location"`
}

// parseCodeClimate reads a JSON array of issues (GitLab) or a stream of
// issue objects separated by NUL or whitespace (the CodeClimate engine
// output).
func parseCodeClimate(data []byte, tool string) ([]Finding, error) {
	var items []codeClimateIssue
	if data[0] == '[' {
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, fmt.Errorf("codeclimate: %w", err)
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(bytes.ReplaceAll(data, []byte{0}, []byte{'\n'})))
		for {
			var it codeClimateIssue
			err := dec.Decode(&it)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("codeclimate: %w", err)
			}
			items = append(items, it)
		}
	}
	var out []Finding
	for _, it := range items {
		if it.Type != "" && !strings.EqualFold(it.Type, "issue") {
			continue
		}
		f := Finding{Tool: tool, Rule: strings.TrimSpace(it.CheckName), Path: it.Location.Path,
			Severity: CodeClimateSeverity(it.Severity), Message: shortText(it.Description, maxMessage)}
		switch l := it.Location; {
		case l.Lines != nil && l.Lines.Begin > 0:
			f.Line, f.EndLine = l.Lines.Begin, l.Lines.End
		case l.Positions != nil && l.Positions.Begin.Line > 0:
			f.Line, f.EndLine = l.Positions.Begin.Line, l.Positions.End.Line
		}
		if f.EndLine <= f.Line {
			f.EndLine = 0
		}
		if f.Path == "" || f.Line <= 0 {
			continue // not tied to a line: cannot be on a changed line
		}
		if f.Rule == "" {
			f.Rule = ciName(tool)
		}
		out = append(out, f)
	}
	return out, nil
}

// CodeClimateSeverity maps CodeClimate / Code Quality severities: blocker
// and critical -> critical, major -> major, minor (and missing / unknown)
// -> minor, info -> nit.
func CodeClimateSeverity(s string) core.Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "blocker", "critical":
		return core.SeverityCritical
	case "major":
		return core.SeverityMajor
	case "info":
		return core.SeverityNit
	}
	return core.SeverityMinor
}

// ---------------------------------------------------------------------------
// SARIF 2.1.0
// ---------------------------------------------------------------------------

// SARIF 2.1.0: runs[].results[] with ruleId, level, message.text and
// locations[].physicalLocation{artifactLocation.uri, region.startLine,
// region.endLine}. Rule help URIs come from runs[].tool.driver.rules[].
type sarifLog struct {
	Runs []struct {
		Tool struct {
			Driver struct {
				Rules []struct {
					ID      string `json:"id"`
					HelpURI string `json:"helpUri"`
				} `json:"rules"`
			} `json:"driver"`
		} `json:"tool"`
		Results []struct {
			RuleID  string `json:"ruleId"`
			Level   string `json:"level"`
			Kind    string `json:"kind"`
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
			Locations []struct {
				PhysicalLocation struct {
					ArtifactLocation struct {
						URI string `json:"uri"`
					} `json:"artifactLocation"`
					Region struct {
						StartLine int `json:"startLine"`
						EndLine   int `json:"endLine"`
					} `json:"region"`
				} `json:"physicalLocation"`
			} `json:"locations"`
		} `json:"results"`
	} `json:"runs"`
}

func parseSARIF(data []byte, tool string) ([]Finding, error) {
	var lg sarifLog
	if err := json.Unmarshal(data, &lg); err != nil {
		return nil, fmt.Errorf("sarif: %w", err)
	}
	if lg.Runs == nil {
		return nil, errors.New("sarif: no runs")
	}
	var out []Finding
	for _, run := range lg.Runs {
		help := map[string]string{}
		for _, r := range run.Tool.Driver.Rules {
			if isHTTPURL(r.HelpURI) {
				help[r.ID] = strings.TrimSpace(r.HelpURI)
			}
		}
		for _, r := range run.Results {
			if r.Kind != "" && r.Kind != "fail" {
				continue // pass, notApplicable, informational, review, open
			}
			if len(r.Locations) == 0 {
				continue // not tied to a line: cannot be on a changed line
			}
			loc := r.Locations[0].PhysicalLocation
			f := Finding{Tool: tool, Rule: r.RuleID, Path: loc.ArtifactLocation.URI, Line: loc.Region.StartLine,
				Severity: sarifSeverity(r.Level), Message: shortText(r.Message.Text, maxMessage), URL: help[r.RuleID]}
			if loc.Region.EndLine > f.Line {
				f.EndLine = loc.Region.EndLine
			}
			if f.Path == "" || f.Line <= 0 {
				continue
			}
			if f.Rule == "" {
				f.Rule = ciName(tool)
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// sarifSeverity maps SARIF levels (default "warning").
func sarifSeverity(level string) core.Severity {
	switch strings.ToLower(level) {
	case "error":
		return core.SeverityMajor
	case "note", "none":
		return core.SeverityNit
	}
	return core.SeverityMinor
}

func isHTTPURL(s string) bool {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, " \t\n<>()[]`\"'") || len(s) > 300 {
		return false
	}
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// ---------------------------------------------------------------------------
// Checkstyle XML
// ---------------------------------------------------------------------------

// Checkstyle XML: <checkstyle><file name><error line column severity
// message source/></file></checkstyle> (phpcs, deptrac, eslint, ktlint, …).
type checkstyleXML struct {
	XMLName xml.Name `xml:"checkstyle"`
	Files   []struct {
		Name   string `xml:"name,attr"`
		Errors []struct {
			Line     int    `xml:"line,attr"`
			Severity string `xml:"severity,attr"`
			Message  string `xml:"message,attr"`
			Source   string `xml:"source,attr"`
		} `xml:"error"`
	} `xml:"file"`
}

func parseCheckstyle(data []byte, tool string) ([]Finding, error) {
	var cs checkstyleXML
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	if err := dec.Decode(&cs); err != nil {
		return nil, fmt.Errorf("checkstyle: %w", err)
	}
	var out []Finding
	for _, f := range cs.Files {
		for _, e := range f.Errors {
			if f.Name == "" || e.Line <= 0 {
				continue
			}
			rule := e.Source
			if rule == "" {
				rule = ciName(tool)
			}
			out = append(out, Finding{Tool: tool, Rule: rule, Path: f.Name, Line: e.Line,
				Severity: checkstyleSeverity(e.Severity), Message: shortText(e.Message, maxMessage)})
		}
	}
	return out, nil
}

func checkstyleSeverity(s string) core.Severity {
	switch strings.ToLower(s) {
	case "error":
		return core.SeverityMajor
	case "info", "ignore":
		return core.SeverityNit
	}
	return core.SeverityMinor
}

// ---------------------------------------------------------------------------
// From reports to findings
// ---------------------------------------------------------------------------

// CIReport is what the review package downloaded for one lint.ci entry.
type CIReport struct {
	// Tool is CIPrefix + the entry's name.
	Tool string
	// Format: FormatAuto (or ""), FormatCodeClimate, FormatSARIF,
	// FormatCheckstyle.
	Format string
	// Severity, when set, applies to every result (before overrides).
	Severity core.Severity
	// Blocking: results follow the normal severity gate.
	Blocking bool
	// Data holds one report per matched job (parallel jobs).
	Data [][]byte
}

// CheckReport parses the reports of one CI linter and keeps the results on
// the lines the MR adds (changed), with severity overrides applied and at
// most maxFindingsPerTool results. The status is StateRan when every
// report was parsed, StatePartial when some were (old threads then stay
// open), StateFailed when none was.
func CheckReport(rep CIReport, changed *Changed, over map[string]core.Severity) ([]Finding, ToolStatus) {
	st := ToolStatus{Tool: rep.Tool}
	var all []Finding
	parsed, bad := 0, ""
	for _, d := range rep.Data {
		fs, err := ParseReport(d, rep.Format, rep.Tool)
		if err != nil {
			bad = shortText(err.Error(), 160)
			continue
		}
		parsed++
		all = append(all, fs...)
	}
	switch {
	case parsed == 0 && bad == "":
		st.State, st.Reason = StateFailed, "отчёт не получен"
		return nil, st
	case parsed == 0:
		st.State, st.Reason = StateFailed, "не удалось разобрать отчёт: "+bad
		return nil, st
	}
	st.State = StateRan
	if bad != "" {
		st.State, st.Reason = StatePartial, "часть отчётов не разобрана: "+bad
	}
	seen := map[string]bool{}
	var uniq []Finding
	for _, f := range all {
		f.Path = ResolveReportPath(f.Path, changed)
		if rep.Severity != "" {
			f.Severity = rep.Severity
		}
		f.MayBlock = rep.Blocking
		if k := f.ID() + fmt.Sprint(":", f.Line); !seen[k] {
			seen[k] = true // parallel jobs may report the same result
			uniq = append(uniq, f)
		}
	}
	st.Total = len(uniq)
	fs := applySeverity(keep(uniq, changed), over)
	sortFindings(fs)
	fs = capFindings(fs, maxFindingsPerTool, &st)
	st.Found = len(fs)
	return fs, st
}

// ResolveReportPath maps a path from a CI report to the repository path:
// "file://" and "./" prefixes are dropped, and an absolute path of the CI
// runner (/builds/group/project/src/a.go) becomes the changed file it ends
// with (src/a.go).
func ResolveReportPath(p string, changed *Changed) string {
	p = strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(p, "\\", "/")), "file://")
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	if changed.Has(p) {
		return p
	}
	best := ""
	for _, f := range changed.Files() {
		if strings.HasSuffix(p, "/"+f) && len(f) > len(best) {
			best = f
		}
	}
	if best != "" {
		return best
	}
	return strings.TrimPrefix(p, "/")
}

// ChangedFromFileDiffs builds the changed lines from the MR diff entries
// (GitLab /diffs), skipping deleted files and paths skip rejects.
func ChangedFromFileDiffs(diffs []core.FileDiff, skip func(path string) bool) *Changed {
	var b strings.Builder
	for _, d := range diffs {
		if d.DeletedFile || d.NewPath == "" || (skip != nil && skip(d.NewPath)) {
			continue
		}
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n+++ b/%s\n", d.NewPath, d.NewPath, d.NewPath)
		b.WriteString(strings.TrimRight(d.Diff, "\n"))
		b.WriteString("\n")
	}
	return ParseChangedLines(b.String())
}
