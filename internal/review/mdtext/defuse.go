package mdtext

import (
	"regexp"
	"strings"
)

// NeutralizeQuickActions escapes GitLab quick actions ("/merge", "/approve",
// "/close", ...) in text that is posted as the bot. Claude's output can be
// steered by prompt injection from the MR, and GitLab executes quick actions
// found at the start of a line in notes with the bot's (Developer) rights.
// Lines inside fenced code blocks are left alone: GitLab ignores quick
// actions there, and suggestions must stay byte-exact. "\/" renders as "/".
// Fences are recognized by CommonMark's rules (see fencedLines): a looser
// rule would make the bot skip a line that GitLab treats as text.
func NeutralizeQuickActions(s string) string {
	lines := strings.Split(s, "\n")
	code := fencedLines(lines)
	for i, l := range lines {
		if code[i] {
			continue
		}
		t := strings.TrimLeft(l, " \t")
		if strings.HasPrefix(t, "/") {
			lines[i] = l[:len(l)-len(t)] + "\\" + t
		}
	}
	return strings.Join(lines, "\n")
}

// fencedLines marks the lines of closed fenced code blocks. As in
// CommonMark, a fence is 3+ backticks or tildes indented by at most three
// spaces (a backtick fence's info string has no backtick), and only a fence
// of the same character, at least as long, followed by nothing but spaces
// closes it: "~~~" does not close "```", "```" does not close "````" (the
// bot's suggestion fences are longer than any backtick run inside them). A
// block that is never closed marks nothing: GitLab versions that find quick
// actions with regular expressions skip closed blocks only.
func fencedLines(lines []string) []bool {
	code := make([]bool, len(lines))
	open, ch, n := -1, byte(0), 0
	for i, l := range lines {
		c, k, rest, ok := fenceLine(l)
		switch {
		case !ok:
		case open < 0:
			if c != '`' || !strings.Contains(rest, "`") {
				open, ch, n = i, c, k
			}
		case c == ch && k >= n && strings.TrimRight(rest, " \t\r") == "":
			for j := open; j <= i; j++ {
				code[j] = true
			}
			open = -1
		}
	}
	return code
}

// fenceLine reports whether l starts with a code fence: its character, its
// length and the rest of the line.
func fenceLine(l string) (ch byte, n int, rest string, ok bool) {
	ind := 0
	for ind < len(l) && l[ind] == ' ' {
		ind++
	}
	if ind > 3 || ind == len(l) || (l[ind] != '`' && l[ind] != '~') {
		return 0, 0, "", false
	}
	l = l[ind:]
	ch = l[0]
	for n < len(l) && l[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, "", false
	}
	return ch, n, l[n:], true
}

// untrustedTagRe is an opening or closing untrusted tag in any letter case
// (a model reads "</UNTRUSTED>" as a closing tag too).
var untrustedTagRe = regexp.MustCompile(`(?i)</?untrusted`)

// Untrusted neutralizes anything that could close the delimiting tags:
// "</untrusted" becomes "<\/untrusted" and "<untrusted" "<\untrusted".
func Untrusted(s string) string {
	return untrustedTagRe.ReplaceAllStringFunc(s, func(m string) string { return `<\` + m[1:] })
}

// DefuseComments breaks every "<!--" of untrusted text ("<!" + U+200B +
// "--"), so that it can neither open an HTML comment nor be read as a bot
// marker by the regular expressions that find the bot's blocks (a fake
// "<!-- /reviewbot:ci -->" in a job log would end the CI block early, and the
// rest of the log would stay in the summary as live Markdown). Inside code
// blocks the zero-width space is invisible.
func DefuseComments(s string) string {
	for strings.Contains(s, "<!--") {
		s = strings.ReplaceAll(s, "<!--", "<!​--")
	}
	return s
}

// mentionRe: an @mention, or a #12 / !12 reference (GitLab would add a
// "mentioned in" note to that issue or MR).
var mentionRe = regexp.MustCompile(`@[A-Za-z0-9_]|[#!][0-9]`)

// DefuseMentions breaks @mentions and #/! references in the model's text
// outside inline code (prompt injection from a job log must not ping
// anybody): "@x" becomes "@\u200bx". Only a real code span is left as it is
// (GitLab expands nothing there): a backtick run closed by a run of the
// same length on the same line, as in CommonMark. An unpaired backtick, a
// backslash-escaped one or one inside something that looks like an HTML
// tag or an autolink ("<\u2026>") opens nothing; when in doubt the text is
// treated as text (a stray zero-width space in code is harmless, a live
// mention is not).
func DefuseMentions(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = defuseMentionsLine(l)
	}
	return strings.Join(lines, "\n")
}

func defuseMentionsLine(l string) string {
	defuse := func(t string) string {
		return mentionRe.ReplaceAllStringFunc(t, func(m string) string { return m[:1] + "\u200b" + m[1:] })
	}
	var b strings.Builder
	text := 0 // start of the text not written yet
	i := 0
	for i < len(l) {
		switch l[i] {
		case '\\':
			i++
			if i < len(l) && l[i] == '`' {
				i += backtickRun(l, i) // an escaped backtick run opens nothing
			} else if i < len(l) {
				i++
			}
		case '<':
			if j := strings.IndexByte(l[i+1:], '>'); j >= 0 {
				i += j + 2 // an HTML tag or autolink: backticks inside are not code
			} else {
				i++
			}
		case '`':
			n := backtickRun(l, i)
			end := closingRun(l, i+n, n)
			if end < 0 {
				i += n // unpaired: literal backticks, the rest is text
				continue
			}
			b.WriteString(defuse(l[text:i]))
			b.WriteString(l[i : end+n])
			i = end + n
			text = i
		default:
			i++
		}
	}
	b.WriteString(defuse(l[text:]))
	return b.String()
}

// backtickRun is the length of the backtick run at l[i].
func backtickRun(l string, i int) int {
	n := 0
	for i+n < len(l) && l[i+n] == '`' {
		n++
	}
	return n
}

// closingRun finds, from l[from], a backtick run of exactly n (-1: none).
func closingRun(l string, from, n int) int {
	for i := from; i < len(l); {
		if l[i] != '`' {
			i++
			continue
		}
		k := backtickRun(l, i)
		if k == n {
			return i
		}
		i += k
	}
	return -1
}
