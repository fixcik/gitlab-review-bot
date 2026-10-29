package finding

import (
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// NoteBody renders the first note of a finding thread.
// snappedFrom is the original line when the anchor was moved (0 otherwise).
func NoteBody(id string, f output.Finding, withSuggestion bool, snappedFrom int) string {
	return NoteBodyOpts(id, f, withSuggestion, snappedFrom, NoteOpts{})
}

// NoteOpts are the merge-gate details of a finding note.
type NoteOpts struct {
	Blocking    bool // the thread blocks the merge until resolved
	NonBlocking bool // say explicitly that it does not block (it gets resolved)
	OutsideDiff bool // posted as a general thread: name the file and line
}

func NoteBodyOpts(id string, f output.Finding, withSuggestion bool, snappedFrom int, o NoteOpts) string {
	var b strings.Builder
	b.WriteString(marker.Finding(id))
	b.WriteString("\n")
	if o.Blocking {
		b.WriteString(marker.Blocking)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "**%s** · %s\n", Badge(f.Severity), mdtext.OneLine(f.Title))
	switch {
	case o.Blocking:
		b.WriteString("\n⛔ **Блокирует мерж.** Исправьте и закройте тред.\n")
	case o.NonBlocking:
		b.WriteString("\n_Не блокирует мерж._\n")
	}
	if o.OutsideDiff {
		fmt.Fprintf(&b, "\n_Файл %s (вне диффа)._\n", mdtext.FileLineRU(f.Path, f.Line))
	}
	if snappedFrom > 0 {
		fmt.Fprintf(&b, "\n_Относится к строке %d._\n", snappedFrom)
	}
	if f.Body != "" {
		b.WriteString("\n")
		b.WriteString(f.Body)
		b.WriteString("\n")
	}
	if withSuggestion && f.Suggestion != "" {
		n := f.EndLine - f.Line
		if n < 0 {
			n = 0
		}
		fence := mdtext.CodeFence(f.Suggestion)
		fmt.Fprintf(&b, "\n%ssuggestion:-0+%d\n%s\n%s\n", fence, n, strings.TrimRight(f.Suggestion, "\n"), fence)
	}
	return mdtext.NeutralizeQuickActions(b.String())
}

// ParseNote extracts title and body from a finding note we wrote.
func ParseNote(body string) (title, text string) {
	lines := strings.Split(body, "\n")
	i := 0
	for ; i < len(lines); i++ {
		if marker.FindingRe.MatchString(lines[i]) {
			i++
			break
		}
	}
	if i >= len(lines) {
		i = 0
	}
	for ; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.HasPrefix(l, "<!--") { // blank or another marker (blocking)
			continue
		}
		if k := strings.Index(l, " · "); k >= 0 {
			title = strings.TrimSpace(l[k+len(" · "):])
		} else {
			title = l
		}
		i++
		break
	}
	var rest []string
	for ; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "```") && strings.Contains(t, "suggestion") {
			break
		}
		rest = append(rest, lines[i])
	}
	return title, strings.TrimSpace(strings.Join(rest, "\n"))
}
