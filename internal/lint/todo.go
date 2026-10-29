package lint

// The built-in "todo" check (review.todo): a TODO / FIXME / XXX / HACK
// comment added by the merge request must reference a task — a Jira key, a
// GitLab issue ("#123", "group/project#123") or a link to an issue — so the
// deferred work is not lost. It runs in the bot itself: no sandbox, no
// external binary, no Claude.
//
// Detection is a per-line lexer chosen by the file name (see syntaxFor):
// it knows the comment syntax of the language (//, #, /* */, --, <!-- -->,
// ;, {# #}, %, …) and skips string literals, so `"TODO list"` in code is
// not a marker. Block comments, Go/JS backtick strings and Python
// triple-quoted strings are tracked across lines; Python docstrings (a
// triple-quoted string that starts a line) count as comments. Known limits
// of a line heuristic: heredocs, raw strings of other languages (r"…",
// """…""" in Kotlin/Scala, $'…'), regex literals containing quotes or
// comment starters, and nested block comments are not understood; a marker
// may then be missed or, rarely, found in a string. Files are read at the
// MR head; when that fails only the lines of the diff are scanned (then a
// "* " line after a gap is taken for a docblock line). Rust/F# lifetimes
// ('a) are not quotes.

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// ToolTodo is the tool name of the todo check (also the thread marker).
const ToolTodo = "todo"

// RuleTodoTaskNotFound is the rule of a referenced Jira key that Jira does
// not know (verify_keys). Markers without a task use the keyword as rule.
const RuleTodoTaskNotFound = "task-not-found"

const (
	// todoMaxFindings caps the todo results of one review.
	todoMaxFindings = 100
	// todoMaxFile: larger files are scanned from the diff only.
	todoMaxFile = 4 << 20
	// todoMaxComment caps the comment text kept in a result.
	todoMaxComment = 200
	// todoGeneratedLines: a "generated" header is looked for in this many
	// first lines.
	todoGeneratedLines = 20
)

// TodoConfig is the effective todo configuration of one review.
type TodoConfig struct {
	// Keywords are the markers (uppercase, matched case-sensitively as
	// whole words).
	Keywords []string
	// RequireTask: a marker without a task reference is a finding.
	RequireTask bool
	Severity    core.Severity
	// Blocking: results follow review.block_min_severity (Finding.MayBlock).
	Blocking bool
	// Ignore are doublestar globs of files not to check.
	Ignore []string
	// ProjectKeys are the configured Jira project keys; empty = any key
	// of the form ABC-123 counts.
	ProjectKeys []string
	// CheckKey verifies referenced keys (verify_keys); nil = not verified.
	CheckKey KeyChecker
}

// TodoRequest is one todo run.
type TodoRequest struct {
	Checkout core.Checkout
	// Head is the MR head (files are read there).
	Head string
	// Diff is `git diff <merge base>..HEAD` with the review ignore applied:
	// only added lines are checked.
	Diff   string
	Config TodoConfig
}

// TodoInfo is what a todo result says, for the thread text.
type TodoInfo struct {
	Keyword string // TODO, FIXME, …
	Comment string // the comment from the keyword on (untrusted)
	// Key is the referenced key Jira does not know (task-not-found).
	Key string
	// ProjectKeys are the configured Jira project keys (the key format).
	ProjectKeys []string
}

// TodoTitle is the thread title of a todo result.
func TodoTitle(f Finding) string {
	if f.Todo == nil {
		return f.Rule
	}
	if f.Rule == RuleTodoTaskNotFound {
		return "задача " + f.Todo.Key + " не найдена"
	}
	return f.Todo.Keyword + " без задачи"
}

// CheckTodo finds task markers without a task reference on the lines the
// MR adds. It never fails: problems are the status.
func CheckTodo(ctx context.Context, req TodoRequest) ([]Finding, ToolStatus) {
	st := ToolStatus{Tool: ToolTodo, State: StateRan}
	cfg := req.Config
	ch := ParseChangedLines(req.Diff)
	files := ch.Files()
	if len(files) == 0 || len(cfg.Keywords) == 0 || (!cfg.RequireTask && cfg.CheckKey == nil) {
		st.State = StateNotApplicable
		return nil, st
	}
	sparse := diffNewLines(req.Diff)
	var out []Finding
	var withKeys []keyedTodo
	for _, p := range files {
		if ctx.Err() != nil {
			st.State, st.Reason = StatePartial, "проверка прервана"
			break
		}
		fs, ks := todoScanFile(ctx, req, p, ch.lines[p], sparse[p])
		out = append(out, fs...)
		withKeys = append(withKeys, ks...)
	}
	if len(withKeys) > 0 {
		missing, failed := verifyTodoKeys(ctx, cfg.CheckKey, withKeys)
		out = append(out, missing...)
		if failed > 0 && st.State == StateRan {
			st.State, st.Reason = StatePartial, fmt.Sprintf("не удалось проверить в Jira ключей задач: %d", failed)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	out = capFindings(out, todoMaxFindings, &st)
	st.Found, st.Total = len(out), len(out)
	return out, st
}

// todoScanFile checks the added lines of one file: markers without a task
// (findings) and markers whose only references are Jira keys to verify.
func todoScanFile(ctx context.Context, req TodoRequest, p string, added map[int]bool, fromDiff map[int]string) ([]Finding, []keyedTodo) {
	cfg := req.Config
	if todoIgnored(p, cfg.Ignore) {
		return nil, nil
	}
	syn := syntaxFor(p)
	if syn == nil {
		return nil, nil
	}
	lines, ok := todoFileLines(ctx, req, p, fromDiff)
	if !ok || generatedFile(lines) {
		return nil, nil
	}
	sc := scanComments(lines, syn)
	changed := make([]int, 0, len(added))
	for n := range added {
		changed = append(changed, n)
	}
	sort.Ints(changed)
	var out []Finding
	var withKeys []keyedTodo
	for _, n := range changed {
		if n < 1 || n >= len(sc) {
			continue
		}
		kw, text, found := findMarker(sc[n].comments, cfg.Keywords)
		if !found {
			continue
		}
		ref := findTaskRef(todoRefText(sc, n, cfg.Keywords), cfg.ProjectKeys, cfg.Keywords)
		info := &TodoInfo{Keyword: kw, Comment: text, ProjectKeys: append([]string(nil), cfg.ProjectKeys...)}
		switch {
		case !ref.any():
			if cfg.RequireTask {
				out = append(out, Finding{Tool: ToolTodo, Rule: kw, Path: p, Line: n, Severity: cfg.Severity,
					Message: kw + " без задачи — «" + text + "»", InDiff: true, MayBlock: cfg.Blocking, Todo: info})
			}
		case cfg.CheckKey != nil && ref.onlyKeys():
			withKeys = append(withKeys, keyedTodo{Finding{Tool: ToolTodo, Rule: RuleTodoTaskNotFound, Path: p, Line: n,
				Severity: core.SeverityMinor, InDiff: true, Todo: info}, ref.keys})
		}
	}
	return out, withKeys
}

// todoRefText is the comment text of line n searched for a task reference:
// the next line too when it continues the comment without a marker of its
// own.
func todoRefText(sc []lineScan, n int, keywords []string) string {
	text := strings.Join(sc[n].comments, " ")
	if n+1 < len(sc) && sc[n+1].commentOnly() {
		if _, _, again := findMarker(sc[n+1].comments, keywords); !again {
			text += " " + strings.Join(sc[n+1].comments, " ")
		}
	}
	return text
}

// todoIgnored reports whether path matches one of the ignore globs (a glob
// without "/" also matches the base name).
func todoIgnored(p string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, p); ok {
			return true
		}
		if !strings.Contains(g, "/") {
			if ok, _ := doublestar.Match(g, path.Base(p)); ok {
				return true
			}
		}
	}
	return false
}

// todoFileLines returns the lines of path at the head, 1-based (index 0
// unused), or the lines the diff shows when the file cannot be read.
// ok=false: a binary file.
func todoFileLines(ctx context.Context, req TodoRequest, p string, fromDiff map[int]string) ([]srcLine, bool) {
	if req.Checkout != nil {
		b, err := req.Checkout.ShowFile(ctx, req.Head, p)
		if err == nil && len(b) <= todoMaxFile {
			if binary(b) {
				return nil, false
			}
			return splitLines(string(b)), true
		}
	}
	maxN := 0
	for n := range fromDiff {
		maxN = max(maxN, n)
	}
	out := make([]srcLine, maxN+2)
	for n, t := range fromDiff {
		if strings.IndexByte(t, 0) >= 0 {
			return nil, false
		}
		out[n] = srcLine{text: t, ok: true}
	}
	return out, true
}

// diffNewLines returns the text of every new-file line the diff shows
// (added and context lines), per path.
func diffNewLines(diff string) map[string]map[int]string {
	out := map[string]map[int]string{}
	p := ""
	n, left := 0, 0
	inHunk := false
	for _, raw := range strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git "):
			p, inHunk = "", false
			continue
		case !inHunk && strings.HasPrefix(raw, "+++ "):
			p = diffPath(raw[4:])
			continue
		case !inHunk && strings.HasPrefix(raw, "--- "):
			continue
		}
		if m := hunkRe.FindStringSubmatch(raw); m != nil {
			n, left, inHunk = atoi(m[1]), 1, true
			if m[2] != "" {
				left = atoi(m[2])
			}
			continue
		}
		if !inHunk || p == "" {
			continue
		}
		switch {
		case strings.HasPrefix(raw, "-"), strings.HasPrefix(raw, `\`):
		case (strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, " ")) && left > 0:
			if out[p] == nil {
				out[p] = map[int]string{}
			}
			out[p][n] = raw[1:]
			n++
			left--
		case raw == "" && left > 0:
			n++
			left--
		default:
			inHunk = false
		}
	}
	return out
}

var generatedRe = regexp.MustCompile(`(?i)code generated .*do not edit|@generated\b|<auto-generated|\bauto-?generated (?:file|code|by)\b|\bthis file (?:is|was) (?:auto(?:matically|-)? ?)?generated\b|\bgenerated by .*do not (?:edit|modify)\b`)

// generatedFile reports whether one of the first lines carries a
// "generated, do not edit" header (Go's "// Code generated … DO NOT
// EDIT.", "@generated", "<auto-generated>", …).
func generatedFile(lines []srcLine) bool {
	for i := 1; i < len(lines) && i <= todoGeneratedLines; i++ {
		if lines[i].ok && generatedRe.MatchString(lines[i].text) {
			return true
		}
	}
	return false
}
