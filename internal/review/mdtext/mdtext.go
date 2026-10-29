// Package mdtext holds the bot's generic text and GitLab Markdown helpers:
// Russian plurals, shortening, code spans and fences, and the defusing of
// Untrusted text. It is a leaf: it must not depend on internal/review or any
// of its feature packages.
package mdtext

import (
	"fmt"
	"strings"
)

// Plural picks the Russian plural form for n: one (1, 21), few (2-4, 22-24),
// many (0, 5-20, 25-30...).
func Plural(n int, one, few, many string) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return few
	}
	return many
}

// LinesWord is the plural form of «строка» for n.
func LinesWord(n int) string { return Plural(n, "строка", "строки", "строк") }

// CapList keeps the first n of ss and replaces the rest with "и ещё N".
func CapList(ss []string, n int) []string {
	if len(ss) <= n {
		return ss
	}
	return append(append([]string(nil), ss[:n]...), fmt.Sprintf("и ещё %d", len(ss)-n))
}

// OneLine collapses all whitespace runs of s into single spaces.
func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Shorten is OneLine capped at n runes ("…" marks a cut).
func Shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// TruncateKeepLines truncates while keeping newlines.
func TruncateKeepLines(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "\n…"
}

func ShortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func Indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
