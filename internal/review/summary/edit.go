package summary

import (
	"regexp"
	"slices"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

// SetStatus inserts (or replaces) a status line such as "⏳ Ревью
// отложено…" in an existing summary body, or builds a minimal summary.
func SetStatus(existing, status, sha, mention string) string {
	if existing == "" {
		return marker.Summary(sha) + "\n" + Title + "\n\n" + status + "\n\n" + Footer("", "", false, mention)
	}
	out := RemoveLines(strings.Split(existing, "\n"), IsStatusLine)
	at := slices.IndexFunc(out, IsTitle)
	switch {
	case at >= 0:
		out = slices.Insert(out, at+1, "", status)
	case len(out) > 0:
		// Unknown layout: put the status right after the marker line.
		out = slices.Insert(out, 1, "", status, "")
	}
	return strings.Join(out, "\n")
}

// TrivialStatusPrefix starts the summary status line (IsStatusLine).
const TrivialStatusPrefix = "ℹ️ Инкремент"

// IsTitle reports whether l is the summary heading line.
func IsTitle(l string) bool { return strings.TrimSpace(l) == Title }

// IsStatusLine reports whether l is a status line (SetStatus).
func IsStatusLine(l string) bool {
	t := strings.TrimSpace(l)
	return strings.HasPrefix(t, "⏳ Ревью отложено") || strings.HasPrefix(t, "❌ Ревью не удалось") || strings.HasPrefix(t, TrivialStatusPrefix)
}

// SwapBlock swaps the block re matches in a summary body for block
// ("" removes it). A new block goes at the first anchor that is in the body,
// else at the end.
func SwapBlock(body, block string, re *regexp.Regexp, anchors ...Anchor) string {
	if loc := re.FindStringIndex(body); loc != nil {
		return body[:loc[0]] + block + body[loc[1]:]
	}
	if block == "" {
		return body
	}
	for _, at := range anchors {
		if out, ok := at(body, block); ok {
			return out
		}
	}
	return strings.TrimRight(body, "\n") + "\n\n" + block
}

// Anchor inserts a new summary block at its place (ok false when the
// place is not in body).
type Anchor func(body, block string) (out string, ok bool)

// AfterTBD: right after the trunk-based block.
func AfterTBD(body, block string) (string, bool) {
	loc := marker.TBDRe.FindStringIndex(body)
	if loc == nil {
		return "", false
	}
	return body[:loc[1]] + "\n\n" + block + body[loc[1]:], true
}

// BeforeCompat: right before the compat section.
func BeforeCompat(body, block string) (string, bool) {
	i := strings.Index(body, CompatSectionTitle)
	if i < 0 {
		return "", false
	}
	return body[:i] + block + "\n\n" + body[i:], true
}

// CompatSectionTitle heads the compat section (an anchor for BeforeCompat).
const CompatSectionTitle = "### 🧩 Обратная совместимость"

// BeforeFooter: right before the summary footer.
func BeforeFooter(body, block string) (string, bool) {
	i := strings.LastIndex(body, "\n---\n")
	if i < 0 {
		return "", false
	}
	return body[:i] + "\n\n" + block + "\n" + body[i:], true
}

// InsertAfter inserts a blank line and block after lines[at], then a blank
// line unless one follows already.
func InsertAfter(lines []string, at int, block string) []string {
	ins := []string{"", block}
	if rest := lines[at+1:]; len(rest) > 0 && strings.TrimSpace(rest[0]) != "" {
		ins = append(ins, "")
	}
	return slices.Insert(lines, at+1, ins...)
}

// RemoveLines drops the lines that match, each with the blank line right
// after it.
func RemoveLines(lines []string, match func(string) bool) []string {
	out := make([]string, 0, len(lines))
	skipBlank := false
	for _, l := range lines {
		switch {
		case match(l):
			skipBlank = true
		case skipBlank && strings.TrimSpace(l) == "":
			skipBlank = false
		default:
			skipBlank = false
			out = append(out, l)
		}
	}
	return out
}
