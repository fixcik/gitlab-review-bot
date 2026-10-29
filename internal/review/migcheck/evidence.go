package migcheck

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/migrations"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// NonBlockingCeiling is the highest severity that does not block the merge
// ("major" with the default block_min_severity).
func (mf *Feature) NonBlockingCeiling() core.Severity {
	for _, s := range []core.Severity{core.SeverityMajor, core.SeverityMinor, core.SeverityNit} {
		if !mf.env.Blocking(s) {
			return s
		}
	}
	return core.SeverityNit
}

// EvidenceChecker reads merge-base files for deployed_usage checks, with a
// read budget and a per-path cache.
type EvidenceChecker struct {
	co    core.Checkout
	run   *Run
	reads int
	lines map[string][]string
	errs  map[string]error
}

// NewEvidenceChecker checks deployed_usage against the merge base of m in co.
func NewEvidenceChecker(co core.Checkout, m *Run) *EvidenceChecker {
	return &EvidenceChecker{co: co, run: m, lines: map[string][]string{}, errs: map[string]error{}}
}

// dropWordRe: a drop or rename on a migration line, in SQL or in a
// framework call (DROP COLUMN, drop_column, dropColumn, RenameField,
// RemoveField, DeleteModel, ...).
var dropWordRe = regexp.MustCompile(`(?i)drop|rename|remove|deletemodel`)

var usageNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{2,128}$`)

// CleanRepoPath reports a relative path inside the repository.
func CleanRepoPath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// FileLines are the lines of a merge-base file the checker read (nil when
// it did not read it or the read failed).
func (e *EvidenceChecker) FileLines(path string) []string {
	return e.lines[path]
}

// Check verifies the evidence of a blocking finding on a migration file
// ("" = confirmed, otherwise why not); ctx bounds the merge-base reads.
func (e *EvidenceChecker) Check(ctx context.Context, f output.Finding) string {
	u := f.DeployedUsage
	switch {
	case u == nil:
		return "no deployed_usage"
	case !CleanRepoPath(u.Path):
		return "deployed_usage path is not a clean repository path"
	case e.run.cfg.IsMigrationPath(u.Path) || e.run.Res.IsMigration(u.Path):
		return "deployed_usage points at a migration"
	case !migrations.IsSourceFile(u.Path):
		return "deployed_usage is not application code (test, vendored or unsupported file)"
	case u.Line < 1:
		return "deployed_usage line < 1"
	case !usageNameRe.MatchString(u.Name):
		return "deployed_usage name is not an identifier"
	case !e.dropsName(f, u.Name):
		return "deployed_usage name is not what the migration drops or renames at the finding's lines"
	}
	lines, err := e.file(ctx, u.Path)
	switch {
	case err != nil:
		return "merge-base file: " + mdtext.OneLine(err.Error())
	case u.Line > len(lines):
		return "deployed_usage line is past the end of the merge-base file"
	case !migrations.UsesName(lines[u.Line-1], u.Name):
		return "the merge-base line does not use deployed_usage name"
	}
	return ""
}

// Verdict applies "only explicit breakage blocks": applies is true for a
// blocking finding on a migration file of this merge request, and then why
// says why its deployed_usage is not confirmed ("" = confirmed). Shared by
// Enforce (after the run) and the MCP tool submit_finding (during the run).
func (mf *Feature) Verdict(ctx context.Context, e *EvidenceChecker, f output.Finding) (applies bool, why string) {
	if e == nil || e.run == nil || !e.run.Res.IsMigration(f.Path) || !mf.env.Blocking(f.Severity) {
		return false, ""
	}
	return true, e.Check(ctx, f)
}

// dropsName reports whether name is the object the migration drops or
// renames at the finding's lines (line..end_line). When the analyzer found
// a drop or rename there, only its object counts: the column of a column
// operation (not its table or the table's entity), the table of a table
// operation (with its entity and camelCase variants), the old name of a
// rename. Otherwise (statements the analyzer does not parse, such as DO
// blocks) those lines must drop or rename something (dropWordRe) and the
// name must appear on them as written or in its snake_case form; the
// table-to-entity variants do not count there.
func (e *EvidenceChecker) dropsName(f output.Finding, name string) bool {
	end := max(f.EndLine, f.Line)
	var objects []string
	for _, op := range e.run.Res.Findings {
		if op.Path != f.Path || op.Line < f.Line || op.Line > end {
			continue
		}
		if r := op.Rule(); r == nil || !r.Contract {
			continue
		}
		obj := op.Column
		if obj == "" {
			obj = op.Table
		}
		if obj != "" {
			objects = append(objects, obj)
		}
	}
	if len(objects) > 0 {
		return slices.ContainsFunc(objects, func(obj string) bool { return migrations.MentionsName(obj, name) })
	}
	lines := strings.Split(e.run.Heads[f.Path], "\n")
	if f.Line < 1 || f.Line > len(lines) {
		return false
	}
	text := strings.Join(lines[f.Line-1:min(end, len(lines))], "\n")
	if !dropWordRe.MatchString(text) {
		return false
	}
	words := regexp.QuoteMeta(name)
	if s := snakeName(name); s != name {
		words += "|" + regexp.QuoteMeta(s)
	}
	return regexp.MustCompile(`(?i)\b(?:` + words + `)\b`).MatchString(text)
}

// snakeName is the snake_case form of a camelCase or PascalCase identifier
// (createdBy -> created_by).
func snakeName(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1])) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func (e *EvidenceChecker) file(ctx context.Context, p string) ([]string, error) {
	if l, ok := e.lines[p]; ok {
		return l, nil
	}
	if err, ok := e.errs[p]; ok {
		return nil, err
	}
	if e.reads >= maxEvidenceReads {
		return nil, fmt.Errorf("check budget exhausted (%d files)", maxEvidenceReads)
	}
	e.reads++
	b, err := e.co.ShowFile(ctx, e.run.Base, p)
	if err != nil {
		e.errs[p] = err
		return nil, err
	}
	l := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	e.lines[p] = l
	return l, nil
}

// Enforce applies "only explicit breakage blocks" to Claude's
// output: a blocking finding on a migration file of this merge request
// stands only when its deployed_usage is confirmed in the merge base
// (otherwise it is downgraded to the highest non-blocking severity); a
// breaking-change item on a migration file (which always blocks) becomes a
// finding at that severity. deployed_usage is dropped everywhere else. It
// returns how many findings were changed. lang is the review language.
func (mf *Feature) Enforce(ctx context.Context, log *slog.Logger, co core.Checkout, out *output.ReviewOutput, m *Run, lang string) (n int) {
	if out == nil {
		return 0
	}
	if m == nil {
		for i := range out.Findings {
			out.Findings[i].DeployedUsage = nil
		}
		return 0
	}
	ceiling := mf.NonBlockingCeiling()
	// Breaking-change items on migration files never block (the model is
	// told to report migrations as findings): converted first, so a later
	// failure cannot leave them blocking.
	if c := out.Compatibility; c != nil {
		kept := c.Breaking[:0]
		for _, b := range c.Breaking {
			if !m.Res.IsMigration(b.Path) {
				kept = append(kept, b)
				continue
			}
			log.Info("review: migrations: breaking change on a migration file turned into a finding",
				"path", b.Path, "line", b.Line, "severity", ceiling)
			out.Findings = append(out.Findings, compatToFinding(b, ceiling))
			n++
		}
		c.Breaking = kept
	}
	// Fail closed: if the evidence check breaks, blocking findings on
	// migration files that were not confirmed yet are downgraded.
	confirmed := map[int]bool{}
	defer func() {
		if p := recover(); p != nil {
			log.Error("review: migrations: enforcement panicked, unconfirmed blocking findings downgraded", "panic", p)
			jobctx.BotProblem(ctx, problems.EvidenceFailed, "проверка deployed_usage: внутренняя ошибка", fmt.Sprint(p))
			for i := range out.Findings {
				f := &out.Findings[i]
				if !confirmed[i] && m.Res.IsMigration(f.Path) && mf.env.Blocking(f.Severity) {
					f.Severity, f.DeployedUsage = ceiling, nil
					n++
				}
			}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, migEvidenceTimeout)
	defer cancel()
	e := NewEvidenceChecker(co, m)
	for i := range out.Findings {
		f := &out.Findings[i]
		applies, why := mf.Verdict(ctx, e, *f)
		if !applies {
			f.DeployedUsage = nil
			continue
		}
		if why != "" {
			attrs := []any{"path", f.Path, "line", f.Line, "from", f.Severity, "to", ceiling, "reason", why}
			if u := f.DeployedUsage; u != nil {
				attrs = append(attrs, "usage_path", u.Path, "usage_line", u.Line, "usage_name", u.Name)
			}
			log.Info("review: migrations: blocking finding without confirmed deployed-code breakage downgraded", attrs...)
			f.Severity, f.DeployedUsage = ceiling, nil
			n++
			continue
		}
		confirmed[i] = true
		if m.proven == nil {
			m.proven = map[string]bool{}
		}
		m.proven[finding.ID(f.Path, f.Title)] = true
		u := f.DeployedUsage
		loc := u.Path + ":" + strconv.Itoa(u.Line)
		log.Info("review: migrations: blocking finding confirmed by deployed usage", "path", f.Path, "line", f.Line, "usage", loc, "name", u.Name)
		if !strings.Contains(f.Body, loc) {
			label := "Used by the deployed code: "
			if lang == "Russian" {
				label = "Использование в задеплоенном коде: "
			}
			f.Body = strings.TrimSpace(f.Body + "\n\n" + label + mdtext.CodeSpan(loc))
		}
	}
	return n
}

func compatToFinding(b output.BreakingChange, sev core.Severity) output.Finding {
	var parts []string
	for _, p := range []struct{ label, text string }{
		{"", b.Explanation}, {"Было: ", b.PreviousBehavior}, {"Стало: ", b.NewBehavior}, {"Как безопаснее: ", b.SafeAlternative},
	} {
		if p.text != "" {
			parts = append(parts, p.label+p.text)
		}
	}
	return output.Finding{Path: b.Path, Line: b.Line, EndLine: b.Line, Severity: sev, Title: b.Title, Body: strings.Join(parts, "\n\n")}
}
