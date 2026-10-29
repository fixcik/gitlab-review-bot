package linters

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// SecretWordsRe matches wording about a secret (token, password, key...): a
// Claude finding next to a leaked secret that says so repeats it.
var SecretWordsRe = regexp.MustCompile(`(?i)secret|секрет|token|токен|password|парол|api[ _-]?key|credential|ключ`)

// DropDuplicates removes Claude findings that repeat a lint result: same
// file, the lint line inside the finding's lines, and the finding names the
// rule or the tool (for secrets: talks about a secret/token/password at all).
// Claude was told not to repeat them; this is the safety net, so a deeper
// finding about the same line that does not restate the tool stays. A
// finding is never dropped in favour of a weaker result: one of a higher
// severity, or one that blocks the merge while the result does not (gate),
// stays (see DuplicateOf).
func DropDuplicates(fs []output.Finding, lr *Run, gate Gate) []output.Finding {
	if lr == nil || lr.Res == nil || len(lr.Res.Findings) == 0 {
		return fs
	}
	out := fs[:0:0]
	for _, f := range fs {
		if DuplicateOf(f, lr.Res.Findings, gate) == nil {
			out = append(out, f)
		}
	}
	return out
}

// genericToolNames are CI tool names that are ordinary words (the default
// tool name is the job name, often just "lint"): a finding that says
// "lint" or "todo" does not name the tool.
var genericToolNames = map[string]bool{
	"lint": true, "lints": true, "linter": true, "linters": true, "linting": true, "todo": true,
	"check": true, "checks": true, "test": true, "tests": true, "ci": true, "build": true,
	"sast": true, "quality": true, "codequality": true, "code-quality": true, "code_quality": true,
	"static": true, "analysis": true, "static-analysis": true, "style": true, "format": true, "fmt": true,
}

// DuplicateOf returns the lint result f repeats (see DropDuplicates),
// nil when none. submit_finding uses it to say which result it repeats.
// The finding must name the rule id or the tool as a whole word (generic
// tool names never count). A result weaker than the finding is never its
// duplicate: a lower severity, or not blocking while the finding blocks
// (gate; nil = severities only). So a critical finding that mentions the
// TODO next to it is not swallowed by the minor todo thread.
func DuplicateOf(f output.Finding, lfs []lint.Finding, gate Gate) *lint.Finding {
	end := f.EndLine
	if end < f.Line {
		end = f.Line
	}
	text := strings.ToLower(f.Title + "\n" + f.Body)
	for i := range lfs {
		l := &lfs[i]
		if l.Path != f.Path || l.Line < f.Line-1 || l.Line > end+1 {
			continue
		}
		if f.Severity != "" && finding.Rank(f.Severity) < finding.Rank(l.Severity) {
			continue // the finding is more severe than the result
		}
		if gate != nil && gate.Blocking(f.Severity) && !gate.ResultBlocking(*l) {
			continue // the finding blocks the merge, the result does not
		}
		if l.Secret() {
			if SecretWordsRe.MatchString(text) {
				return l
			}
			continue
		}
		rule := strings.ToLower(l.Rule)
		name := strings.TrimPrefix(l.Tool, lint.CIPrefix) // "ci:ruff" -> "ruff"
		if i := strings.LastIndex(rule, "."); i >= 0 && name == "semgrep" {
			rule = rule[i+1:] // semgrep: the last part of the dotted id
		}
		if genericToolNames[name] {
			name = ""
		}
		if containsWord(text, rule) || containsWord(text, name) {
			return l
		}
	}
	return nil
}

// containsWord reports whether text contains w (both lowercase) as a whole
// word: not preceded or followed by a letter, digit, '_' or '-' (so "unused"
// does not match "unused_var" and "exec" does not match "dangerous-exec").
func containsWord(text, w string) bool {
	if w == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(text[from:], w)
		if i < 0 {
			return false
		}
		i += from
		j := i + len(w)
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[j:])
		if (i == 0 || !wordRune(before)) && (j == len(text) || !wordRune(after)) {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		from = i + size
	}
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' }
