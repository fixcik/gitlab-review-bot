// Package marker holds every "<!-- reviewbot:… -->" marker the bot writes
// into its notes: the constants and regular expressions, and the functions
// that build, find, parse and strip them. It is a leaf: it must not depend
// on internal/review or any of its feature packages.
package marker

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	SummaryRe = regexp.MustCompile(`<!--\s*reviewbot:summary\s+sha=([0-9a-fA-F]*)\s*-->`)
	FindingRe = regexp.MustCompile(`<!--\s*reviewbot:finding\s+id=([0-9a-fA-F]+)\s*-->`)
)

// Summary is the marker of the summary note for the reviewed head sha.
func Summary(sha string) string { return fmt.Sprintf("<!-- reviewbot:summary sha=%s -->", sha) }

// Finding is the marker of a finding thread.
func Finding(id string) string { return fmt.Sprintf("<!-- reviewbot:finding id=%s -->", id) }

// ParseFinding returns the finding id of a note's marker header.
func ParseFinding(body string) (string, bool) {
	m := FindingRe.FindStringSubmatch(Header(body))
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

// IsSummaryNote reports whether the note's marker header has the summary
// marker.
func IsSummaryNote(body string) bool { return SummaryRe.MatchString(Header(body)) }

// Header returns the leading lines of a note body that are HTML
// comments. The bot writes its markers (finding, compat, lint, blocking,
// fixed, summary) there and nowhere else, while the rest of a bot note
// carries untrusted text: Claude's answers, CI linter reports and checker
// output. Markers are only recognized in the header, so such text cannot
// make a thread look fixed, blocking or like another kind of thread.
func Header(body string) string {
	end := 0
	for end < len(body) {
		nl := strings.IndexByte(body[end:], '\n')
		line := body[end:]
		if nl >= 0 {
			line = body[end : end+nl]
		}
		if !strings.HasPrefix(strings.TrimSpace(line), "<!--") {
			break
		}
		if nl < 0 {
			end = len(body)
			break
		}
		end += nl + 1
	}
	return body[:end]
}

// Fixed tags the bot's "fixed" reply in a finding thread.
const Fixed = "<!-- reviewbot:fixed -->"

// Blocking tags finding threads that block the merge.
const Blocking = "<!-- reviewbot:blocking -->"

// HasFixed / HasBlocking look at the marker header only.
func HasFixed(body string) bool { return strings.Contains(Header(body), Fixed) }

func HasBlocking(body string) bool { return strings.Contains(Header(body), Blocking) }

// BotRe matches any reviewbot marker comment.
var BotRe = regexp.MustCompile(`(?s)<!--\s*/?reviewbot:.*?-->`)

// StripBot removes reviewbot marker comments from untrusted text
// that is posted as the bot (Claude's chat reply may start with one). It
// repeats until nothing is removed: a marker split by another one
// ("<!-<!--reviewbot:x-->- reviewbot:ci …-->") reassembles when the inner
// one goes.
func StripBot(s string) string {
	for {
		out := BotRe.ReplaceAllString(s, "")
		if out == s {
			return out
		}
		s = out
	}
}

// StripNoteMarkers removes the finding, compat, lint and summary markers
// from a note (anywhere in it) and trims the result.
func StripNoteMarkers(s string) string {
	s = FindingRe.ReplaceAllString(s, "")
	s = CompatRe.ReplaceAllString(s, "")
	s = LintRe.ReplaceAllString(s, "")
	s = SummaryRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// BotThreadRe matches the marker of a thread the bot opened for a
// remark: findings, breaking changes, lint and any later kind that follows
// the "<!-- reviewbot:<kind> ... -->" convention.
var BotThreadRe = regexp.MustCompile(`<!--\s*reviewbot:[a-z]`)

// SummaryAnyRe is the summary marker with any sha value.
var SummaryAnyRe = regexp.MustCompile(`<!--\s*reviewbot:summary\s+sha=[^\s>]*\s*-->`)

// SetSummarySHA points the summary marker at sha (the reviewed head).
func SetSummarySHA(body, sha string) string {
	if loc := SummaryAnyRe.FindStringIndex(body); loc != nil {
		return body[:loc[0]] + Summary(sha) + body[loc[1]:]
	}
	return body
}

// ReplaceBlock swaps the block from start to end (markers included)
// for block; body unchanged when it has no such block.
func ReplaceBlock(body, start, end, block string) string {
	i := strings.Index(body, start)
	if i < 0 {
		return body
	}
	j := strings.Index(body[i:], end)
	if j < 0 {
		return body
	}
	return body[:i] + block + body[i+j+len(end):]
}
