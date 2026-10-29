package mdtext

import (
	"cmp"
	"fmt"
	"strings"
)

// The code-span and table-cell helpers below carry repository paths,
// branch names, rules and model text into notes. Their output never has a
// "<!--" (DefuseComments): a summary note's blocks are found again with
// regular expressions over the whole body, and a fake
// "<!-- reviewbot:ci … -->" in a file name would be read as the bot's own.

// InlineCode wraps s in a code span that survives backticks inside.
func InlineCode(s string) string {
	s = DefuseComments(OneLine(s))
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// InlineCodeList renders names as inline code joined with ", ", at most n of them
// plus "и ещё N".
func InlineCodeList(names []string, n int) string {
	out := make([]string, 0, len(names))
	for _, s := range names {
		out = append(out, InlineCode(s))
	}
	return strings.Join(CapList(out, n), ", ")
}

// CodeSpan wraps repository text as inline code (references are not
// expanded inside code; backticks cannot close it).
func CodeSpan(s string) string {
	return "`" + DefuseComments(strings.ReplaceAll(Shorten(s, 200), "`", "'")) + "`"
}

// Code renders a value (a path, a branch, a rule, a commit) as one line of
// inline code: whitespace collapsed, backticks dropped so that the span
// cannot be closed early (a branch "main`@all`x" would otherwise mention
// everybody), "<!--" defused; "`—`" for an empty value.
func Code(s string) string {
	return "`" + cmp.Or(DefuseComments(strings.ReplaceAll(OneLine(s), "`", "")), "—") + "`"
}

// CodeFence returns a backtick fence longer than any run inside s.
func CodeFence(s string) string {
	longest, cur := 0, 0
	for _, r := range s {
		if r == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	if longest < 3 {
		return "```"
	}
	return strings.Repeat("`", longest+1)
}

// TableCell makes text safe for a Markdown table cell.
func TableCell(s string) string {
	s = DefuseComments(OneLine(s))
	return strings.ReplaceAll(s, "|", `\|`)
}

// PathLine is "path:line", or the path alone without a line.
func PathLine(path string, line int) string {
	if line > 0 {
		return fmt.Sprintf("%s:%d", path, line)
	}
	return path
}

// FileLineRU names a location in a note: "`path`, строка N" (the path alone
// without a line).
func FileLineRU(path string, line int) string {
	loc := "`" + DefuseComments(strings.ReplaceAll(path, "`", "")) + "`"
	if line > 0 {
		loc += fmt.Sprintf(", строка %d", line)
	}
	return loc
}

// CodeLoc is "`path:line`" for a summary line: "—" without a path, no line
// when it is 0, backticks dropped so the code span cannot break.
func CodeLoc(path string, line int) string {
	loc := cmp.Or(path, "—")
	if line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, line)
	}
	return "`" + DefuseComments(strings.ReplaceAll(loc, "`", "")) + "`"
}

// Inline makes third-party text (Jira) safe for one line of GitLab
// Markdown: one line, shortened, Markdown/HTML metacharacters escaped and
// @mentions/#references broken so that nobody gets pinged.
func Inline(s string, n int) string { return Escape(Shorten(s, n)) }

// Escape is Inline without shortening (s must already be one line).
func Escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '`', '*', '_', '[', ']', '<', '>', '|', '~', '!':
			b.WriteRune('\\')
			b.WriteRune(r)
		case '@', '#', '&', '%', '$':
			b.WriteRune(r)
			b.WriteRune('​')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// CodeCell renders one value as inline code safe for a table cell.
func CodeCell(s string) string {
	s = DefuseComments(strings.ReplaceAll(OneLine(s), "`", ""))
	if s == "" {
		return "—"
	}
	return "`" + strings.ReplaceAll(s, "|", `\|`) + "`"
}

// IsHTTPURL reports whether u is an http(s) URL (safe as a Markdown link).
func IsHTTPURL(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}
