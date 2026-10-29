package verify

// Which findings the verification run gets, with their diff excerpts.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// selectFindings picks the findings to verify: new (not an open bot
// thread, first of its id), severity in review.verify.severities, at most
// review.verify.max_findings in order (Findings are sorted by severity).
// Linter duplicates were dropped before. diff is the reviewed diff for the
// excerpts.
//
// proven (may be nil) names the findings the bot already proved
// deterministically (verify/verify_proven.go): they are counted, not verified.
func (vf *Feature) selectFindings(fs []output.Finding, threads *mrthread.Threads, diff string, proven func(output.Finding) string) (items []Item, overflow int, skipped []provenFinding) {
	vc := vf.env.Cfg.Review.Verify
	sevs, max := vc.SeveritySet(), vc.Max()
	seen := map[string]bool{}
	var chunks []diffcap.Chunk
	for i, f := range fs {
		id := finding.ID(f.Path, f.Title)
		if seen[id] {
			continue
		}
		seen[id] = true
		if threads != nil {
			if _, open := threads.Open[id]; open {
				continue
			}
		}
		if !sevs[string(f.Severity)] {
			continue
		}
		if proven != nil {
			if why := proven(f); why != "" {
				skipped = append(skipped, provenFinding{F: f, By: why})
				continue
			}
		}
		if len(items) == max {
			overflow++
			continue
		}
		if chunks == nil {
			chunks = diffcap.Split(diff)
		}
		items = append(items, Item{ID: "F" + strconv.Itoa(len(items)+1), Index: i, F: f, Excerpt: diffExcerpt(chunks, f)})
	}
	return items, overflow, skipped
}

const (
	excerptContext  = 12
	excerptMaxLines = 80
)

// diffExcerpt is the part of the file's diff around the finding's lines,
// new-file line numbers on the left ("" when the file is not in the diff).
func diffExcerpt(chunks []diffcap.Chunk, f output.Finding) string {
	var text string
	found := false
	for _, c := range chunks {
		if c.Path == f.Path {
			text, found = c.Text, true
			break
		}
	}
	if !found {
		return ""
	}
	end := max(f.EndLine, f.Line)
	lo, hi := f.Line-excerptContext, end+excerptContext
	var b strings.Builder
	n, newLine, inHunk, lastHunk := 0, 0, false, ""
	for _, l := range strings.Split(text, "\n") {
		if m := diffcap.HunkRe.FindStringSubmatch(l); m != nil {
			newLine, _ = strconv.Atoi(m[2])
			inHunk, lastHunk = true, l
			continue
		}
		if !inHunk || l == "" || strings.HasPrefix(l, `\`) {
			continue
		}
		num, prefix := newLine, ""
		switch l[0] {
		case '+', ' ':
			prefix = fmt.Sprintf("%5d ", newLine)
			newLine++
		case '-':
			prefix = "      "
		default:
			continue
		}
		if f.Line > 0 && (num < lo || num > hi) {
			continue
		}
		if n == excerptMaxLines {
			b.WriteString("      …\n")
			break
		}
		if lastHunk != "" {
			b.WriteString(lastHunk + "\n")
			lastHunk = ""
			n++
		}
		b.WriteString(prefix + l + "\n")
		n++
	}
	return strings.TrimRight(b.String(), "\n")
}
