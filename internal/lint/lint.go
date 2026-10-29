// Package lint produces the deterministic results the bot publishes itself,
// only on the lines a merge request changes:
//
//   - gitleaks (secrets), the one scanner the bot still runs: through
//     core.Runner.Exec, in the sandbox image, without network and without
//     credentials, argv without a shell, over every commit of the MR;
//   - linter reports produced by the repository's own CI (lint.ci in
//     .reviewbot.yaml): CodeClimate / GitLab Code Quality JSON, SARIF and
//     Checkstyle XML, parsed here (report.go) after the review package
//     downloaded them from the MR head pipeline. The bot does not run
//     linters itself.
//
// A tool that is not installed or fails, or a CI report that is missing, is
// never a finding: it is reported as a ToolStatus and the review goes on.
// The bot never changes the repository.
package lint

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Tool names (also the values of review.lint.tools and lint.disable).
const (
	ToolGitleaks = "gitleaks"
)

// AllTools is every in-process tool in run order (secrets first). Linters
// are not here: their results come from CI reports (CIPrefix).
var AllTools = []string{ToolGitleaks}

// KnownTool reports whether name is a supported tool.
func KnownTool(name string) bool {
	for _, t := range AllTools {
		if t == name {
			return true
		}
	}
	return false
}

// IsLinter reports whether tool is a linter (style/correctness): its
// findings do not block the merge unless the lint.ci entry is marked
// blocking (Finding.MayBlock).
func IsLinter(tool string) bool {
	return IsCI(tool) || tool == ToolTodo
}

// Tool states.
const (
	// StateRan: the tool ran completely; its findings are authoritative.
	StateRan = "ran"
	// StateNotApplicable: nothing for this tool in the MR (no Go module, no
	// changed Python files, ...). Counts as a complete run.
	StateNotApplicable = "n/a"
	// StateUnavailable: the tool is not installed / not set up.
	StateUnavailable = "unavailable"
	// StateFailed: the tool ran but could not produce a usable report.
	StateFailed = "failed"
	// StatePartial: the tool ran on a subset of the files only.
	StatePartial = "partial"
	// StatePending: a CI linter whose job has not finished yet; the bot
	// reads its report when the pipeline finishes (no finding, no problem,
	// nothing is closed meanwhile).
	StatePending = "pending"
)

const (
	defaultTimeout = 3 * time.Minute
	// maxFindingsPerTool caps parsed results per tool (after filtering).
	maxFindingsPerTool = 300
	maxMessage         = 600
)

// Finding is one result on a changed line. It never carries a secret value:
// for gitleaks only the rule, the location and the commit are kept.
type Finding struct {
	Tool     string
	Rule     string
	Path     string // repository path (new file)
	Line     int    // line in the NEW file
	EndLine  int
	Severity core.Severity
	Message  string // tool text (untrusted: derived from repository content)
	URL      string // rule documentation, when the tool gives one
	// Commit is where gitleaks found the secret (short SHA), "" otherwise.
	Commit string
	// InDiff: Line is an added line of the MR diff (gitleaks results may
	// come from a commit whose line was changed again later).
	InDiff bool
	// MayBlock: a CI linter marked blocking (lint.ci[].blocking); its
	// results follow the normal severity gate. Other linters never block.
	MayBlock bool
	// Todo is set on results of the todo check (todo.go), nil otherwise.
	Todo *TodoInfo
}

// Secret reports whether f is a leaked secret (always merge-blocking).
func (f Finding) Secret() bool { return f.Tool == ToolGitleaks }

var digitsRe = regexp.MustCompile(`[0-9]+`)

// ID is the stable id of the finding for thread dedupe: first 12 hex chars of
// sha1(tool, rule, path, normalized message). Line numbers are not part of it
// (digits in the message are normalized too), so the id survives edits above
// the finding and the thread is not re-posted on later reviews.
//
// A gitleaks result is keyed by the commit and the line in that commit
// instead: two secrets of one rule in one file are separate threads, and a
// commit's lines never move (only a history rewrite changes them).
func (f Finding) ID() string {
	if f.Tool == ToolGitleaks {
		return ID(f.Tool, f.Rule, f.Path, fmt.Sprintf("commit %s line %d", f.Commit, f.Line))
	}
	return ID(f.Tool, f.Rule, f.Path, f.Message)
}

// ID hashes the identity of a lint result.
func ID(tool, rule, path, message string) string {
	msg := strings.ToLower(strings.Join(strings.Fields(message), " "))
	if tool != ToolGitleaks {
		msg = digitsRe.ReplaceAllString(msg, "N")
	}
	sum := sha1.Sum([]byte(tool + "\n" + rule + "\n" + path + "\n" + msg))
	return hex.EncodeToString(sum[:])[:12]
}

// ToolStatus says how one tool did.
type ToolStatus struct {
	Tool   string
	State  string
	Reason string // Russian, one line (unavailable / failed / partial)
	// Found is the number of findings on changed lines; Total the number the
	// tool reported before filtering by changed lines.
	Found, Total int
}

// Result of Check.
type Result struct {
	Findings []Finding
	Tools    []ToolStatus
	// Notes are extra warnings for the summary (Russian).
	Notes []string
}

// Status returns the status of tool (ok=false when it was not run at all).
func (r *Result) Status(tool string) (ToolStatus, bool) {
	if r == nil {
		return ToolStatus{}, false
	}
	for _, s := range r.Tools {
		if s.Tool == tool {
			return s, true
		}
	}
	return ToolStatus{}, false
}

// ToolOK reports whether tool ran completely this time (or had nothing to
// check). Only then may its old threads be auto-resolved: a missing or
// failing tool must not silently close a thread.
func (r *Result) ToolOK(tool string) bool {
	s, ok := r.Status(tool)
	return ok && (s.State == StateRan || s.State == StateNotApplicable)
}

// Config is the effective lint configuration of one review.
type Config struct {
	// Tools to run (service list minus what the repository disabled).
	Tools []string
	// SeverityOverrides: "tool" or "tool:rule" -> severity. gitleaks is
	// always critical whatever this says.
	SeverityOverrides map[string]core.Severity
}

// Request is one lint run.
type Request struct {
	Checkout core.Checkout
	// Base is the merge base, Head the MR head: results are limited to the
	// lines added in Base..Head.
	Base, Head string
	// Ignore are the review's ignore globs (not applied to gitleaks: a
	// secret is a secret wherever it is).
	Ignore []string
	Config Config
	// Diff is `git diff Base..HEAD` with Ignore applied, when the caller
	// already has it ("" = computed here).
	Diff string
}

// Checker runs the tools.
type Checker struct {
	Runner  core.Runner
	Timeout time.Duration // per tool run
	Logger  *slog.Logger
}

// Check runs every configured, applicable tool. The error is non-nil only
// when the run could not start (git failure); tool problems are statuses.
func (c *Checker) Check(ctx context.Context, req Request) (*Result, error) {
	log := c.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	co := req.Checkout
	diff := req.Diff
	if diff == "" {
		d, err := co.Diff(ctx, req.Base, req.Ignore)
		if err != nil {
			return nil, fmt.Errorf("lint: diff: %w", err)
		}
		diff = d
	}
	ch := ParseChangedLines(diff)
	files, err := co.ListFiles(ctx, req.Head)
	if err != nil {
		return nil, fmt.Errorf("lint: list files: %w", err)
	}
	env := &runEnv{c: c, req: req, changed: ch, files: files, dir: co.Dir(), log: log}
	if env.timeout = c.Timeout; env.timeout <= 0 {
		env.timeout = defaultTimeout
	}
	res := &Result{}
	for _, tool := range orderTools(req.Config.Tools) {
		var st ToolStatus
		var fs []Finding
		switch tool {
		case ToolGitleaks:
			fs, st = env.gitleaks(ctx)
		default:
			continue
		}
		st.Tool = tool
		fs = capFindings(applySeverity(fs, req.Config.SeverityOverrides), maxFindingsPerTool, &st)
		st.Found = len(fs)
		res.Findings = append(res.Findings, fs...)
		res.Tools = append(res.Tools, st)
		if st.State != StateRan && st.State != StateNotApplicable {
			log.Info("lint: tool issue", "tool", tool, "state", st.State, "reason", st.Reason)
		}
	}
	if ch.Has(".gitleaksignore") && contains(req.Config.Tools, ToolGitleaks) {
		res.Notes = append(res.Notes, "MR меняет `.gitleaksignore`: для проверки взята версия из целевой ветки; новые исключения начнут действовать только после мержа — проверьте, что они не скрывают настоящие секреты.")
	}
	sortFindings(res.Findings)
	return res, nil
}

// capFindings keeps the first n findings; a complete run that loses some
// becomes partial, so its old threads are not closed.
func capFindings(fs []Finding, n int, st *ToolStatus) []Finding {
	if len(fs) <= n {
		return fs
	}
	if st.State == StateRan {
		st.State, st.Reason = StatePartial, fmt.Sprintf("показаны первые %d результатов", n)
	}
	return fs[:n]
}

func orderTools(tools []string) []string {
	want := map[string]bool{}
	for _, t := range tools {
		want[strings.ToLower(strings.TrimSpace(t))] = true
	}
	var out []string
	for _, t := range AllTools {
		if want[t] {
			out = append(out, t)
		}
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if strings.EqualFold(strings.TrimSpace(x), s) {
			return true
		}
	}
	return false
}

func sortFindings(fs []Finding) {
	rank := func(s core.Severity) int {
		switch s {
		case core.SeverityCritical:
			return 0
		case core.SeverityMajor:
			return 1
		case core.SeverityMinor:
			return 2
		}
		return 3
	}
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Secret() != b.Secret() {
			return a.Secret()
		}
		if rank(a.Severity) != rank(b.Severity) {
			return rank(a.Severity) < rank(b.Severity)
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
}

// applySeverity applies severity overrides ("tool:rule" wins over "tool").
// gitleaks results are always critical.
func applySeverity(fs []Finding, over map[string]core.Severity) []Finding {
	for i := range fs {
		f := &fs[i]
		if f.Tool == ToolGitleaks {
			f.Severity = core.SeverityCritical
			continue
		}
		if s, ok := over[f.Tool+":"+f.Rule]; ok && s != "" {
			f.Severity = s
		} else if s, ok := over[f.Tool]; ok && s != "" {
			f.Severity = s
		}
		if f.Severity == "" {
			f.Severity = core.SeverityMinor
		}
	}
	return fs
}

// ---------------------------------------------------------------------------
// Text helpers
// ---------------------------------------------------------------------------

func shortText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }
