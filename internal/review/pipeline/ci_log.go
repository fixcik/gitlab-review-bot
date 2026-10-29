package pipeline

// Cleaning job logs for the digest (cleanCILog) and the fingerprint of a
// failure (ciFingerprint).

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fixcik/gitlab-review-bot/internal/problems"
)

var (
	// ansiRe: CSI sequences (colors, erase line), OSC sequences, other
	// two-byte escapes.
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	// sectionRe: GitLab collapsible section markers.
	sectionRe = regexp.MustCompile(`section_(?:start|end):\d+:[A-Za-z0-9_.\-]+(?:\[[^\]\r\n]*\])?\r?`)
	// tsRe: a leading timestamp (GitLab's timestamped logs: "2026-10-24T10:00:00.123456Z 01O ").
	tsRe = regexp.MustCompile(`^\[?\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?\]?\s+(?:\d{2}[OE]\+?\s)?`)
	// runnerNoiseRe: runner lines that say nothing about the failure.
	runnerNoiseRe = regexp.MustCompile(`^(?:Running after_script|Running after script|Uploading artifacts|WARNING: Uploading artifacts|Uploading artifacts for|Saving cache|Creating cache|Not uploading cache|Cleaning up project directory|Cleaning up file based variables|Skipping Git submodules|Job succeeded|Executing "step_|Preparing environment|Getting source from Git|Fetching changes|Checking out [0-9a-f]+ as|Created fresh repository|Restoring cache|Downloading artifacts|Resolving secrets|Using docker image|Pulling docker image|Running on runner-|Using Docker executor|Starting service|Waiting for services|Preparing the "docker" executor|Reinitialized existing Git repository|Initialized empty Git repository|gitlab-runner |Running with gitlab-runner|  on .* system ID:)`)
)

// cleanCILog turns a raw job log into its last max meaningful lines (max
// <= 0: all): GitLab section markers, ANSI sequences, carriage-return
// progress overwrites and timestamps removed, runner boilerplate and empty
// lines dropped, long lines cut; then secrets are scrubbed.
func cleanCILog(raw string, max int, scrub func(string) string) []string {
	if !utf8.ValidString(raw) {
		raw = strings.ToValidUTF8(raw, "�")
	}
	raw = sectionRe.ReplaceAllString(raw, "")
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	var out []string
	for _, l := range strings.Split(raw, "\n") {
		if i := strings.LastIndexByte(l, '\r'); i >= 0 {
			l = l[i+1:] // the terminal shows what was written last
		}
		l = ansiRe.ReplaceAllString(l, "")
		l = strings.Map(ciLogRune, l)
		l = tsRe.ReplaceAllString(l, "")
		l = strings.TrimRight(l, " ")
		if t := strings.TrimSpace(l); t == "" || runnerNoiseRe.MatchString(t) {
			continue
		}
		if utf8.RuneCountInString(l) > ciMaxLineRunes {
			l = string([]rune(l)[:ciMaxLineRunes]) + "…"
		}
		out = append(out, l)
	}
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	if scrub != nil && len(out) > 0 {
		// Scrubbed as one text: multi-line secrets (private keys) included.
		out = strings.Split(scrub(strings.Join(out, "\n")), "\n")
	}
	return out
}

// ciLogRune turns tabs into spaces and drops other control characters.
func ciLogRune(r rune) rune {
	switch {
	case r == '\t':
		return ' '
	case r < 0x20 || r == 0x7f:
		return -1
	}
	return r
}

// ciFingerprint: job names, failed tests and the normalized last lines of
// each log (numbers, hex ids and temp paths stripped), sha256, 16 hex.
func ciFingerprint(d *ciDigest) string {
	var parts []string
	for _, jd := range d.Failed {
		p := []string{"job " + jd.Job.Name}
		for _, t := range jd.Tests {
			p = append(p, "test "+problems.NormalizeSubject(t.Suite+" "+t.Classname+" "+t.Name))
		}
		tail := jd.Log
		if len(tail) > 8 {
			tail = tail[len(tail)-8:]
		}
		for _, l := range tail {
			p = append(p, "log "+problems.NormalizeSubject(l))
		}
		parts = append(parts, strings.Join(p, "\n"))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n\n")))
	return hex.EncodeToString(sum[:8])
}
