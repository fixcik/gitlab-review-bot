// Package diffmap parses the unified diffs of a merge request and answers
// which lines of the NEW file are visible (and therefore commentable) in the
// GitLab MR diff view.
//
// GitLab accepts a "text" position on a new-file line only when that line is
// shown in the diff: either an added line (anchor with new_line only) or an
// unchanged context line (anchor with both old_line and new_line).
package diffmap

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// lineInfo describes one visible new-file line.
type lineInfo struct {
	added   bool // true: '+' line; false: context line
	oldLine int  // old-file line number for context lines; 0 for added lines
}

type fileMap struct {
	oldPath string
	newPath string
	deleted bool
	lines   map[int]lineInfo // new line -> info
}

// Map indexes the visible new-file lines of every file in an MR diff.
// Files are looked up by their NEW path. A zero or nil *Map is empty.
type Map struct {
	files map[string]*fileMap
	order []string
}

// Build parses diffs into a Map. Malformed lines are ignored; Build never
// fails.
func Build(diffs []core.FileDiff) *Map {
	m := &Map{files: make(map[string]*fileMap, len(diffs))}
	for _, d := range diffs {
		path := d.NewPath
		if path == "" {
			path = d.OldPath
		}
		if path == "" {
			continue
		}
		oldPath := d.OldPath
		if oldPath == "" {
			oldPath = path
		}
		fm, ok := m.files[path]
		if !ok {
			fm = &fileMap{oldPath: oldPath, newPath: path, lines: map[int]lineInfo{}}
			m.files[path] = fm
			m.order = append(m.order, path)
		}
		if d.DeletedFile {
			// A deleted file shows no new-file lines.
			fm.deleted = true
			fm.lines = map[int]lineInfo{}
			continue
		}
		if fm.deleted {
			continue
		}
		parseInto(fm.lines, d.Diff)
	}
	return m
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parseInto walks the hunks of a unified diff and records visible new lines.
func parseInto(lines map[int]lineInfo, diff string) {
	var (
		inHunk           bool
		oldNo, newNo     int // next line numbers
		oldLeft, newLeft int // remaining line counts in the current hunk
	)
	diff = strings.TrimSuffix(strings.ReplaceAll(diff, "\r\n", "\n"), "\n")
	for _, raw := range strings.Split(diff, "\n") {
		if m := hunkRe.FindStringSubmatch(raw); m != nil {
			oldNo = atoi(m[1])
			oldLeft = countOr1(m[2])
			newNo = atoi(m[3])
			newLeft = countOr1(m[4])
			inHunk = oldLeft > 0 || newLeft > 0
			continue
		}
		if !inHunk {
			// File headers ("diff --git", "---", "+++", "index ...") and
			// anything else between hunks.
			continue
		}
		if strings.HasPrefix(raw, `\`) {
			// "\ No newline at end of file" — annotates the previous line.
			continue
		}
		var tag byte = ' '
		if raw != "" {
			tag = raw[0]
		}
		// An empty line inside a hunk is a context line whose leading space
		// was stripped (some tools/editors do this).
		switch tag {
		case '+':
			if newLeft <= 0 {
				inHunk = false
				continue
			}
			lines[newNo] = lineInfo{added: true}
			newNo++
			newLeft--
		case '-':
			if oldLeft <= 0 {
				inHunk = false
				continue
			}
			oldNo++
			oldLeft--
		case ' ':
			if oldLeft <= 0 || newLeft <= 0 {
				inHunk = false
				continue
			}
			lines[newNo] = lineInfo{oldLine: oldNo}
			oldNo++
			newNo++
			oldLeft--
			newLeft--
		default:
			// Unknown line: hunk ended (e.g. the next file's header).
			inHunk = false
			continue
		}
		if oldLeft <= 0 && newLeft <= 0 {
			inHunk = false
		}
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func countOr1(s string) int {
	if s == "" {
		return 1
	}
	return atoi(s)
}

func (m *Map) file(path string) *fileMap {
	if m == nil || m.files == nil {
		return nil
	}
	return m.files[path]
}

// Anchor returns a GitLab text position for newLine of the file whose NEW
// path is path, when that line is visible in the diff. Added lines get only
// NewLine; context lines get OldLine and NewLine.
func (m *Map) Anchor(refs core.DiffRefs, path string, newLine int) (*core.Position, bool) {
	fm := m.file(path)
	if fm == nil {
		return nil, false
	}
	li, ok := fm.lines[newLine]
	if !ok {
		return nil, false
	}
	pos := &core.Position{
		BaseSHA:  refs.BaseSHA,
		StartSHA: refs.StartSHA,
		HeadSHA:  refs.HeadSHA,
		OldPath:  fm.oldPath,
		NewPath:  fm.newPath,
		NewLine:  newLine,
	}
	if !li.added {
		pos.OldLine = li.oldLine
	}
	return pos, true
}

// Snap returns the visible new-file line closest to newLine, at most maxDist
// lines away. At equal distance an added line wins over a context line, and
// then the lower line number wins.
func (m *Map) Snap(path string, newLine, maxDist int) (int, bool) {
	fm := m.file(path)
	if fm == nil || len(fm.lines) == 0 {
		return 0, false
	}
	if maxDist < 0 {
		maxDist = 0
	}
	for d := 0; d <= maxDist; d++ {
		best, found, bestAdded := 0, false, false
		for _, cand := range []int{newLine - d, newLine + d} { // lower first
			li, ok := fm.lines[cand]
			if !ok {
				continue
			}
			if !found || (li.added && !bestAdded) {
				best, found, bestAdded = cand, true, li.added
			}
		}
		if found {
			return best, true
		}
	}
	return 0, false
}

// HasFile reports whether path (a NEW path) is part of the diff. Deleted
// files are part of the diff but have no visible new lines.
func (m *Map) HasFile(path string) bool {
	return m.file(path) != nil
}

// Ranges returns the added lines and all visible (commentable: added and
// context) lines of a file as sorted, merged [from, to] ranges of new-file
// line numbers. Both are empty for a file outside the diff or deleted.
func (m *Map) Ranges(path string) (added, visible [][2]int) {
	fm := m.file(path)
	if fm == nil {
		return nil, nil
	}
	lines := make([]int, 0, len(fm.lines))
	for l := range fm.lines {
		lines = append(lines, l)
	}
	sort.Ints(lines)
	add := func(rs [][2]int, l int) [][2]int {
		if n := len(rs); n > 0 && rs[n-1][1] == l-1 {
			rs[n-1][1] = l
			return rs
		}
		return append(rs, [2]int{l, l})
	}
	for _, l := range lines {
		visible = add(visible, l)
		if fm.lines[l].added {
			added = add(added, l)
		}
	}
	return added, visible
}

// Files returns the NEW paths of all files in the diff, sorted.
func (m *Map) Files() []string {
	if m == nil {
		return nil
	}
	out := append([]string(nil), m.order...)
	sort.Strings(out)
	return out
}
