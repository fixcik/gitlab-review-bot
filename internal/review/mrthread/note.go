package mrthread

import (
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// NotePathLine is where an inline note sits: the new path and line (the old
// path for a deleted file); "", 0 for a general note.
func NotePathLine(n core.Note) (path string, line int) {
	p := n.Position
	if p == nil {
		return "", 0
	}
	if p.NewPath == "" {
		return p.OldPath, p.NewLine
	}
	return p.NewPath, p.NewLine
}

// NoteLocation is the file and line of an inline note as shown to a model:
// the new side, or the old path and line when the note has no new path
// (unlike NotePathLine, which keeps the new line); "", 0 for a general note.
func NoteLocation(n core.Note) (path string, line int) {
	p := n.Position
	switch {
	case p == nil:
		return "", 0
	case p.NewPath == "":
		return p.OldPath, p.OldLine
	}
	return p.NewPath, p.NewLine
}

// ParseCompatNote extracts the title line of a compat thread we wrote.
func ParseCompatNote(body string) string {
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "<!--") {
			continue
		}
		if k := strings.Index(l, " · "); k >= 0 {
			return strings.TrimSpace(l[k+len(" · "):])
		}
		return l
	}
	return ""
}
