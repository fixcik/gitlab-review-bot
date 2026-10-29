package marker

import (
	"fmt"
	"regexp"
	"strings"
)

// Thread markers: the first line of a bot thread says what it is about.

// CompatRe is the marker of a breaking-change thread.
var CompatRe = regexp.MustCompile(`<!--\s*reviewbot:compat\s+id=([0-9a-fA-F]+)\s+source=([a-z-]+)\s*-->`)

// Compat is the marker of a breaking-change thread.
func Compat(id, source string) string {
	return fmt.Sprintf("<!-- reviewbot:compat id=%s source=%s -->", id, source)
}

// ParseCompat returns the id and source of a breaking-change thread's
// marker (marker header only).
func ParseCompat(body string) (id, source string, ok bool) {
	m := CompatRe.FindStringSubmatch(Header(body))
	if m == nil {
		return "", "", false
	}
	return strings.ToLower(m[1]), m[2], true
}

// LintRe is the marker of a lint thread.
var LintRe = regexp.MustCompile(`<!--\s*reviewbot:lint\s+id=([0-9a-fA-F]+)\s+tool=([a-z0-9:._-]+)\s*-->`)

// Lint is the marker of a lint thread.
func Lint(id, tool string) string {
	return fmt.Sprintf("<!-- reviewbot:lint id=%s tool=%s -->", id, tool)
}

// ParseLint returns the id and tool of a lint thread's marker (marker
// header only).
func ParseLint(body string) (id, tool string, ok bool) {
	m := LintRe.FindStringSubmatch(Header(body))
	if m == nil {
		return "", "", false
	}
	return strings.ToLower(m[1]), m[2], true
}

// TooBigThread heads the merge-blocking thread (review.big_mr.block).
const TooBigThread = "<!-- reviewbot:too-big-gate -->"

// CIThreadPrefix starts the marker of the CI explanation thread.
const CIThreadPrefix = "reviewbot:ci-explain"

// CIThreadRe parses the marker of the CI explanation thread.
var CIThreadRe = regexp.MustCompile(`<!--\s*reviewbot:ci-explain\s+pipeline=(\d+)\s+fp=([0-9a-f-]*)\s+explained=(\d+)\s+state=(red|green)\s*-->`)

// CIThread is the marker of the CI explanation thread.
func CIThread(pipeline int64, fp string, explained int64, green bool) string {
	state := "red"
	if green {
		state = "green"
	}
	return fmt.Sprintf("<!-- %s pipeline=%d fp=%s explained=%d state=%s -->", CIThreadPrefix, pipeline, FP(fp), explained, state)
}

// FP is a fingerprint as written in a marker ("-" for none).
func FP(fp string) string {
	if fp == "" {
		return "-"
	}
	return fp
}

// CIMainNotePrefix starts the marker of a main branch pipeline note.
const CIMainNotePrefix = "reviewbot:ci-main"

// CIMainNoteRe parses the marker of a main branch pipeline note.
var CIMainNoteRe = regexp.MustCompile(`<!--\s*reviewbot:ci-main\s+pipeline=(\d+)\s+state=(red|green)\s*-->`)

// ConfigNote tags the bot's config note; state=error|ok says whether
// it currently shows errors.
const ConfigNote = "<!-- reviewbot:config-error"

// ConfigNoteRe parses the config note marker.
var ConfigNoteRe = regexp.MustCompile(`<!--\s*reviewbot:config-error(?:\s+state=(\w+))?\s*-->`)

// ProblemsNotify tags the separate note of notify_mode: note.
const ProblemsNotify = "<!-- reviewbot:problems-notify -->"
