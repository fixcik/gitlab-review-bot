package mcp

// submit_finding: in-loop validation of one finding with the functions the
// post-run pipeline uses, in the same order:
//
//	finding.Normalize (finding.NormalizeReview) -> migcheck.Feature.Verdict
//	(migcheck.Feature.Enforce) -> linters.DuplicateOf (linters.DropDuplicates)
//	-> open threads and the cap (review/review_publish.go publishFindings,
//	finding.NormalizeReview) -> publish.Publisher.Place (publishFindings)
//
// so a finding the tool accepts is published the way the tool said, and one
// it rejects would be dropped after the run as well. The post-run pipeline
// still runs everything again on the merged findings.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
)

const (
	maxTitleRunes = 300
	maxBodyRunes  = 8000
)

// Verdict statuses of submit_finding.
const (
	verdictAccepted = "accepted"
	verdictAdjusted = "adjusted"
	verdictRejected = "rejected"
)

type submitArgs struct {
	Path          string           `json:"path"`
	Line          int              `json:"line"`
	EndLine       int              `json:"end_line"`
	Severity      string           `json:"severity"`
	Title         string           `json:"title"`
	Body          string           `json:"body"`
	Suggestion    string           `json:"suggestion"`
	DeployedUsage *output.UsageRef `json:"deployed_usage"`
}

// submitSchema is the input schema of submit_finding (deployed_usage only
// when the merge request has migrations).
func (m *Run) submitSchema() json.RawMessage {
	lang := m.in.Lang
	if lang == "" {
		lang = "the review language"
	}
	props := map[string]any{
		"path":       map[string]any{"type": "string", "description": "File path relative to the repository root (new path)."},
		"line":       map[string]any{"type": "integer", "minimum": 0, "description": "First line in the NEW file; a changed line (see diff_lines). 0 = the file as a whole (listed in the summary, not inline)."},
		"end_line":   map[string]any{"type": "integer", "minimum": 0, "description": "Last line in the NEW file covered by the finding (>= line); default: line."},
		"severity":   map[string]any{"type": "string", "enum": []string{"critical", "major", "minor", "nit"}},
		"title":      map[string]any{"type": "string", "maxLength": maxTitleRunes, "description": "One-line summary of the problem, in " + lang + "."},
		"body":       map[string]any{"type": "string", "maxLength": maxBodyRunes, "description": "Why it is a problem and how to fix it (Markdown), in " + lang + "."},
		"suggestion": map[string]any{"type": "string", "maxLength": maxBodyRunes, "description": "Optional exact replacement text for lines line..end_line of the new file, without code fences."},
	}
	if m.in.Migrations != nil {
		var u map[string]any
		if err := json.Unmarshal([]byte(migcheck.DeployedUsageSchema), &u); err == nil {
			props["deployed_usage"] = u
		}
	}
	b, _ := json.Marshal(map[string]any{
		"type": "object", "additionalProperties": false,
		"required":   []string{"path", "line", "severity", "title", "body"},
		"properties": props,
	})
	return b
}

// verdict is the answer of submit_finding.
type verdict struct {
	Status    string        `json:"status"`
	ID        string        `json:"id,omitempty"`
	Replaced  bool          `json:"replaced,omitempty"`
	Severity  core.Severity `json:"severity,omitempty"`
	Placement string        `json:"placement,omitempty"`
	Blocking  bool          `json:"blocking,omitempty"`
	Reasons   []string      `json:"reasons,omitempty"`
	Hint      string        `json:"hint,omitempty"`

	finding output.Finding // the finding as kept (accepted / adjusted)
}

func (v *verdict) reject(reason, hint string) *verdict {
	v.Status, v.Reasons, v.Hint = verdictRejected, append(v.Reasons, reason), hint
	return v
}

func (v *verdict) adjust(reason, hint string) {
	v.Status = verdictAdjusted
	v.Reasons = append(v.Reasons, reason)
	if hint != "" {
		if v.Hint != "" {
			v.Hint += " "
		}
		v.Hint += hint
	}
}

func (m *Run) submit(ctx context.Context, raw json.RawMessage) (string, error) {
	var a submitArgs
	if err := mcpserver.DecodeArgs(raw, &a); err != nil {
		return "", err
	}
	v := m.checkSubmit(ctx, a)
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.verdicts[v.Status]++
	m.mu.Unlock()
	m.log.Info("review: mcp submit_finding", "status", v.Status, "id", v.ID, "path", mdtext.Shorten(a.Path, 200), "line", a.Line,
		"severity", a.Severity, "reasons", mdtext.Shorten(strings.Join(v.Reasons, "; "), 500))
	return string(b), nil
}

// checkInput rejects malformed submissions (what the final JSON schema
// would not allow either).
func checkInput(a submitArgs) (output.Finding, string) {
	p := finding.CleanPath(a.Path)
	switch {
	case p == "":
		return output.Finding{}, "path is required"
	case len(p) > maxPathArg || !migcheck.CleanRepoPath(p):
		return output.Finding{}, fmt.Sprintf("path %q must be relative to the repository root, without \"..\"", mdtext.Shorten(a.Path, 80))
	case a.Line < 0 || a.EndLine < 0:
		return output.Finding{}, "line and end_line must be >= 0"
	case a.EndLine > 0 && a.EndLine < a.Line:
		return output.Finding{}, "end_line must be >= line"
	case strings.TrimSpace(a.Title) == "":
		return output.Finding{}, "title is required"
	case utf8.RuneCountInString(a.Title) > maxTitleRunes:
		return output.Finding{}, fmt.Sprintf("title is longer than %d characters", maxTitleRunes)
	case strings.TrimSpace(a.Body) == "":
		return output.Finding{}, "body is required: say why it is a problem and how to fix it"
	case utf8.RuneCountInString(a.Body) > maxBodyRunes || utf8.RuneCountInString(a.Suggestion) > maxBodyRunes:
		return output.Finding{}, fmt.Sprintf("body and suggestion must be at most %d characters", maxBodyRunes)
	}
	switch core.Severity(strings.ToLower(strings.TrimSpace(a.Severity))) {
	case core.SeverityCritical, core.SeverityMajor, core.SeverityMinor, core.SeverityNit:
	default:
		return output.Finding{}, fmt.Sprintf("severity %q is not one of critical, major, minor, nit", mdtext.Shorten(a.Severity, 20))
	}
	return output.Finding{Path: p, Line: a.Line, EndLine: a.EndLine, Severity: core.Severity(a.Severity), Title: a.Title,
		Body: a.Body, Suggestion: a.Suggestion, DeployedUsage: a.DeployedUsage}, ""
}

// checkSubmit validates one submission and keeps it when not rejected.
func (m *Run) checkSubmit(ctx context.Context, a submitArgs) *verdict {
	v := &verdict{Status: verdictAccepted}
	f, bad := checkInput(a)
	if bad != "" {
		return v.reject(bad, "Fix the arguments and call submit_finding again.")
	}
	f, ok := finding.Normalize(f)
	if !ok {
		return v.reject("title is empty after removing bot markers", "Write a plain one-line title.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f = m.checkMigration(ctx, v, f)
	if r := m.checkDuplicates(v, f); r != nil {
		return r
	}
	m.checkPlacement(v, f)
	return m.keep(v, f)
}

// checkMigration applies migcheck.Feature.Enforce's rule (Verdict).
func (m *Run) checkMigration(ctx context.Context, v *verdict, f output.Finding) output.Finding {
	if m.ev == nil {
		if f.DeployedUsage != nil {
			f.DeployedUsage = nil
			v.adjust("deployed_usage ignored: this merge request has no database migrations", "")
		}
		return f
	}
	applies, why := m.f.migrations.Verdict(ctx, m.ev, f)
	switch {
	case !applies && f.DeployedUsage != nil:
		f.DeployedUsage = nil
		v.adjust("deployed_usage ignored: it only counts for a blocking finding on a migration file", "")
	case applies && why != "":
		ceiling := m.f.migrations.NonBlockingCeiling()
		v.adjust(fmt.Sprintf("%s on the migration file %s blocks the merge only when it breaks the deployed code, proven by deployed_usage; %s: severity lowered to %s",
			f.Severity, f.Path, m.evidenceDetail(f, why), ceiling),
			fmt.Sprintf("To keep %s, submit it again with deployed_usage {path, line, name}: a line of the merge-base (deployed) application code (%s), not a migration or a test, that uses the dropped or renamed object, and the identifier exactly as written there. Otherwise keep it at %s.",
				f.Severity, migcheck.DeployedRead(m.in.Migrations.BaseRoot), ceiling))
		f.Severity, f.DeployedUsage = ceiling, nil
	case applies:
		v.Reasons = append(v.Reasons, fmt.Sprintf("deployed_usage confirmed at %s:%d", f.DeployedUsage.Path, f.DeployedUsage.Line))
	}
	return f
}

// evidenceDetail makes migcheck.EvidenceChecker's reason concrete where it can.
func (m *Run) evidenceDetail(f output.Finding, why string) string {
	u := f.DeployedUsage
	if u == nil {
		return "deployed_usage is missing"
	}
	loc := u.Path + ":" + strconv.Itoa(u.Line)
	switch why {
	case "the merge-base line does not use deployed_usage name":
		s := fmt.Sprintf("the cited line %s at the merge base does not contain %s", loc, u.Name)
		if lines := m.ev.FileLines(u.Path); u.Line >= 1 && u.Line <= len(lines) {
			s += fmt.Sprintf(" (it reads: %q)", mdtext.Shorten(lines[u.Line-1], 160))
		}
		return s
	case "deployed_usage line is past the end of the merge-base file":
		return fmt.Sprintf("%s is past the end of %s at the merge base (%d lines)", loc, u.Path, len(m.ev.FileLines(u.Path)))
	case "deployed_usage name is not what the migration drops or renames at the finding's lines":
		lines := strconv.Itoa(f.Line)
		if f.EndLine > f.Line {
			lines += "-" + strconv.Itoa(f.EndLine)
		}
		return fmt.Sprintf("%s is not what the migration %s drops or renames at line %s (anchor the finding on the line of the drop or rename and name that object)", u.Name, f.Path, lines)
	}
	return why + " (" + loc + ")"
}

// checkDuplicates: linter results (linters.DropDuplicates), threads still open
// from earlier reviews (review.publishFindings skips them) and the findings cap.
func (m *Run) checkDuplicates(v *verdict, f output.Finding) *verdict {
	if lr := m.in.Lint; lr != nil && lr.Res != nil {
		if l := linters.DuplicateOf(f, lr.Res.Findings, m.f.lint); l != nil {
			what := fmt.Sprintf("the %s result %s", l.Tool, mdtext.OneLine(l.Rule))
			if l.Secret() {
				what = "the leaked-secret result of gitleaks"
			}
			return v.reject(fmt.Sprintf("duplicate of %s at %s:%d: the bot publishes linter results itself as their own threads", what, l.Path, l.Line),
				"Do not report linter results again. Submit only a different or deeper problem the tool did not describe, and say what the tool missed.")
		}
	}
	id := finding.ID(f.Path, f.Title)
	if m.in.Threads != nil {
		if _, open := m.in.Threads.Open[id]; open {
			return v.reject(fmt.Sprintf("already reported in a thread that is still open (finding id %s)", id),
				"Do not report it again. If the current code fixes it, list it in \"resolved\" of the final answer.")
		}
	}
	if max := m.f.env.Cfg.Review.MaxFindings; max > 0 && m.draftIndex(id) < 0 && len(m.drafts) >= max {
		return v.reject(fmt.Sprintf("the findings cap of this review (%d) is reached", max),
			"Withdraw a less important finding first (list_findings, withdraw_finding), or leave this one out.")
	}
	return nil
}

// checkPlacement reports where review.publishFindings will put the finding.
func (m *Run) checkPlacement(v *verdict, f output.Finding) {
	pl := m.f.pub.Place(m.idx, m.mr.DiffRefs, f)
	v.Blocking, v.Placement = pl.Blocking, pl.Kind
	if !m.idxOK {
		v.Placement = ""
		v.Reasons = append(v.Reasons, "the merge request diff could not be loaded, so the line was not checked; it is checked when publishing")
		return
	}
	near := m.nearLines(f.Path, f.Line)
	switch pl.Kind {
	case publish.Snapped:
		v.adjust(fmt.Sprintf("line %d is not a commentable line in the diff of %s: the thread will be placed on line %d (within %d lines) with a note",
			f.Line, f.Path, pl.Pos.NewLine, publish.SnapDistance),
			"If the problem is on another changed line, submit it again with that line (the same title replaces it).")
	case publish.General, publish.Summary:
		where := fmt.Sprintf("line %d is not in the diff of %s", f.Line, f.Path)
		switch {
		case f.Line == 0:
			where = "line 0 (the whole file) cannot be commented inline"
		case !m.idx.HasFile(f.Path):
			where = fmt.Sprintf("%s is not part of this merge request's diff", f.Path)
		case near != "":
			where += "; changed lines near it: " + near
		}
		if pl.Kind == publish.General {
			v.adjust(where+": this blocking finding will be posted as a general thread, not on a line",
				"Anchor it on a changed line (diff_lines) if the problem is there.")
		} else {
			v.adjust(where+": this finding will only be listed in the summary, not as a thread",
				"Anchor it on a changed line (diff_lines) to get an inline thread, or keep it as a summary note.")
		}
	}
}

// nearLines lists the added (else commentable) ranges of path closest to
// line, at most four.
func (m *Run) nearLines(path string, line int) string {
	rg, ok := m.idx.(diffRanger)
	if !ok {
		return ""
	}
	added, visible := rg.Ranges(path)
	rs := added
	if len(rs) == 0 {
		rs = visible
	}
	if len(rs) == 0 {
		return ""
	}
	dist := func(r [2]int) int {
		switch {
		case line < r[0]:
			return r[0] - line
		case line > r[1]:
			return line - r[1]
		}
		return 0
	}
	// The four closest, shown in line order. Ranges are sorted and disjoint
	// (diffmap), so a stable sort keeps the earlier of two equally close.
	idx := make([]int, len(rs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return dist(rs[idx[i]]) < dist(rs[idx[j]]) })
	if len(idx) > 4 {
		idx = idx[:4]
	}
	sort.Ints(idx)
	pick := make([][2]int, 0, len(idx))
	for _, i := range idx {
		pick = append(pick, rs[i])
	}
	return rangesText(pick, 4)
}

func (m *Run) draftIndex(id string) int {
	for i, d := range m.drafts {
		if finding.ID(d.f.Path, d.f.Title) == id {
			return i
		}
	}
	return -1
}

// keep stores the finding (replacing one with the same path and title).
func (m *Run) keep(v *verdict, f output.Finding) *verdict {
	v.Severity, v.finding = f.Severity, f
	id := finding.ID(f.Path, f.Title)
	if i := m.draftIndex(id); i >= 0 {
		m.drafts[i].f = f
		v.ID, v.Replaced = m.drafts[i].id, true
		return v
	}
	m.seq++
	v.ID = "F" + strconv.Itoa(m.seq)
	m.drafts = append(m.drafts, draft{id: v.ID, f: f})
	return v
}
