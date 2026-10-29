package diffcap

import (
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// FullDiffBase is where a full review starts: the merge base (BaseSHA), so
// that `git diff from..HEAD` matches the MR diff; StartSHA as fallback.
func FullDiffBase(mr *core.MergeRequest) string {
	if mr.DiffRefs.BaseSHA != "" {
		return mr.DiffRefs.BaseSHA
	}
	return mr.DiffRefs.StartSHA
}

// CountLines counts added and removed lines of a file's diff: GitLab
// /diffs hunks or a `git diff` chunk, whose ---/+++ header lines (before
// the first "@@") are not changes; "\ No newline" lines are skipped. The
// TBD block and the size limit of the review (bigmr.go) count with it.
func CountLines(diff string) (added, removed int) {
	inHunk := false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case !inHunk && (strings.HasPrefix(l, "+++ ") || strings.HasPrefix(l, "--- ")):
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			removed++
		}
	}
	return added, removed
}

// IgnoredPath matches the repo's ignore globs (same rules as the checkout).
func IgnoredPath(p string, globs []string) bool {
	for _, g := range globs {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		if ok, err := doublestar.Match(g, p); err == nil && ok {
			return true
		}
	}
	return false
}

// HunkRe matches a hunk header; groups: old start, new start.
var HunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
