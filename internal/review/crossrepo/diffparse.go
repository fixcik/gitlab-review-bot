package crossrepo

// Changed lines of a `git diff` for the system map: the old-side ranges of
// phase A and the exact added and removed lines of phase B.

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

// oldHunkRe matches a hunk header; groups: old start, old line count.
var oldHunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)

// oldSideRanges returns the changed old-side line ranges per old path of a
// `git diff` (deleted and modified lines; a pure insertion touches the
// lines around it). New files have no old side and are skipped.
func oldSideRanges(diff string) map[string][]sysmap.LineRange {
	out := map[string][]sysmap.LineRange{}
	path := ""
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			path = ""
		case strings.HasPrefix(l, "--- "):
			path = diffPath(l[4:], "a/")
		case strings.HasPrefix(l, "@@ ") && path != "":
			m := oldHunkRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			r := sysmap.LineRange{From: start, To: start + n - 1}
			if n == 0 { // insertion after line start
				r = sysmap.LineRange{From: max(start, 1), To: start + 1}
			}
			out[path] = append(out[path], r)
		}
	}
	return out
}

// diffLines is what phase B takes from the full MR diff.
type diffLines struct {
	added   map[string][]sysmap.LineRange // new path -> added lines
	removed map[string][]sysmap.LineRange // old path -> removed lines
	changed []string                      // new paths of files present at the head
	deleted []string                      // old paths of deleted files
}

// parseDiffLines reads a `git diff`: exact added (new side) and removed
// (old side) lines per file.
func parseDiffLines(diff string) diffLines {
	out := diffLines{added: map[string][]sysmap.LineRange{}, removed: map[string][]sysmap.LineRange{}}
	oldPath, newPath := "", ""
	oldLine, newLine := 0, 0
	inHunk := false
	push := func(m map[string][]sysmap.LineRange, p string, n int) {
		if p == "" {
			return
		}
		rs := m[p]
		if k := len(rs); k > 0 && rs[k-1].To == n-1 {
			rs[k-1].To = n
		} else {
			rs = append(rs, sysmap.LineRange{From: n, To: n})
		}
		m[p] = rs
	}
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			oldPath, newPath, inHunk = "", "", false
			continue
		case !inHunk && strings.HasPrefix(l, "--- "):
			oldPath = diffPath(l[4:], "a/")
			continue
		case !inHunk && strings.HasPrefix(l, "+++ "):
			newPath = diffPath(l[4:], "b/")
			if newPath != "" {
				out.changed = append(out.changed, newPath)
			} else if oldPath != "" {
				out.deleted = append(out.deleted, oldPath)
			}
			continue
		case strings.HasPrefix(l, "@@ "):
			m := diffcap.HunkRe.FindStringSubmatch(l)
			if m == nil {
				inHunk = false
				continue
			}
			oldLine, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[2])
			inHunk = true
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(l, "+"):
			push(out.added, newPath, newLine)
			newLine++
		case strings.HasPrefix(l, "-"):
			push(out.removed, oldPath, oldLine)
			oldLine++
		case strings.HasPrefix(l, `\`):
		default:
			oldLine++
			newLine++
		}
	}
	return out
}

func diffPath(p, prefix string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if !strings.HasPrefix(p, prefix) {
		return ""
	}
	return p[len(prefix):]
}
