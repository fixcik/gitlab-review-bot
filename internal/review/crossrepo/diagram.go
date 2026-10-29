package crossrepo

// Mermaid diagram of the change in the summary note ("🗺 Схема изменений").
// Claude returns raw Mermaid in ReviewOutput.Diagram; the bot validates it
// (allowed diagram types, no interaction/links/directives/HTML, size caps)
// and renders it as a ```mermaid block, which GitLab renders natively. An
// invalid diagram is dropped (logged) and never fails the review.

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

const (
	// maxDiagramLines caps the number of lines of a diagram.
	maxDiagramLines = 60
	// maxDiagramLineRunes caps one line.
	maxDiagramLineRunes = 200
	// diagramCollapseLines: longer diagrams are rendered inside <details>.
	diagramCollapseLines = 20

	diagramTitle = "🗺 Схема изменений"
)

var (
	diagramCodeRe = regexp.MustCompile("(?s)```mermaid\n(.*?)\n```")

	flowchartHeadRe = regexp.MustCompile(`^(?:flowchart|graph)\s+(?:TB|TD|BT|RL|LR)\s*;?$`)
	sequenceHeadRe  = regexp.MustCompile(`^sequenceDiagram\s*$`)
	// Interaction statements: flowchart "click", sequence "link"/"links"
	// (at the start of a statement; ";" separates statements).
	interactionRe = regexp.MustCompile(`(?i)(?:^|;)\s*(?:click|links?)\b`)
	// Callback invocation as in `click A call cb()`.
	callRe = regexp.MustCompile(`(?i)\bcall\s+[\w.$]+\s*\(`)
	// HTML tags: "<" directly followed by an optional "/" and a letter.
	htmlTagRe = regexp.MustCompile(`</?[A-Za-z!?]`)
	// URI schemes that execute or embed content.
	schemeRe = regexp.MustCompile(`(?i)\b(?:javascript|vbscript)\s*:|\bdata:[a-z]+/`)
)

// forbiddenDiagram are case-insensitive substrings that are never allowed.
var forbiddenDiagram = []string{
	"href", "%%{", "<script", "://", "```", "~~~", "<!--",
}

// sanitizeDiagram validates Claude's Mermaid diagram and returns it in the
// form to render. "" with a nil error means no diagram was given.
func sanitizeDiagram(s string) (string, error) {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.TrimSpace(s)
	// Tolerate a ```mermaid fence around the whole diagram.
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 && strings.HasSuffix(s, "```") {
			s = strings.TrimSpace(strings.TrimSuffix(s[i+1:], "```"))
		}
	}
	if s == "" {
		return "", nil
	}
	if !utf8.ValidString(s) {
		return "", errors.New("invalid utf-8")
	}
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	if len(lines) > maxDiagramLines {
		return "", fmt.Errorf("too many lines (%d > %d)", len(lines), maxDiagramLines)
	}
	head := strings.TrimSpace(lines[0])
	if !sequenceHeadRe.MatchString(head) && !flowchartHeadRe.MatchString(head) {
		return "", fmt.Errorf("unsupported diagram type %q", mdtext.Shorten(head, 40))
	}
	for n, l := range lines {
		if utf8.RuneCountInString(l) > maxDiagramLineRunes {
			return "", fmt.Errorf("line %d too long", n+1)
		}
		if strings.ContainsFunc(l, func(r rune) bool { return r < 0x20 && r != '\t' }) {
			return "", fmt.Errorf("line %d: control character", n+1)
		}
		low := strings.ToLower(l)
		for _, bad := range forbiddenDiagram {
			if strings.Contains(low, bad) {
				return "", fmt.Errorf("line %d: forbidden %q", n+1, bad)
			}
		}
		switch {
		case interactionRe.MatchString(l):
			return "", fmt.Errorf("line %d: interaction statement", n+1)
		case schemeRe.MatchString(l):
			return "", fmt.Errorf("line %d: uri scheme", n+1)
		case callRe.MatchString(l):
			return "", fmt.Errorf("line %d: callback", n+1)
		case htmlTagRe.MatchString(l):
			return "", fmt.Errorf("line %d: html tag", n+1)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// renderDiagramSection renders the marker-delimited section ("" for an
// empty diagram). Diagrams longer than diagramCollapseLines are collapsed.
func renderDiagramSection(diagram string) string {
	if diagram == "" {
		return ""
	}
	var b strings.Builder
	if strings.Count(diagram, "\n")+1 > diagramCollapseLines {
		fmt.Fprintf(&b, "<details>\n<summary>%s</summary>\n\n", diagramTitle)
	} else {
		fmt.Fprintf(&b, "### %s\n\n", diagramTitle)
	}
	b.WriteString("```mermaid\n")
	b.WriteString(diagram)
	b.WriteString("\n```\n")
	if strings.Count(diagram, "\n")+1 > diagramCollapseLines {
		b.WriteString("\n</details>\n")
	}
	return summary.Block(marker.DiagramStart+"\n", b.String(), marker.DiagramEnd+"\n\n")
}

// previousDiagram extracts (and re-validates) the diagram of an existing
// summary note ("" when there is none).
func previousDiagram(body string) string {
	block := marker.DiagramRe.FindString(body)
	if block == "" {
		return ""
	}
	m := diagramCodeRe.FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	d, err := sanitizeDiagram(m[1])
	if err != nil {
		return ""
	}
	return d
}

// DiagramSection decides which diagram goes into the summary: Claude's
// (validated); on an incremental review without a (valid) new one, the
// previous summary's. Disabled -> "".
func DiagramSection(log *slog.Logger, eff config.Effective, out *output.ReviewOutput, prevSummary string, incremental bool) string {
	if !eff.Diagram || out == nil {
		return ""
	}
	d, err := sanitizeDiagram(out.Diagram)
	if err != nil {
		log.Warn("review: diagram rejected", "err", err)
		d = ""
	}
	if d == "" && incremental {
		d = previousDiagram(prevSummary)
	}
	return renderDiagramSection(d)
}

// DiagramRules is appended to the review system prompt when the diagram is
// enabled ("" otherwise). It is the same for full and incremental reviews
// (prompt caching): the incremental rule is conditional on what the user
// prompt says about the diff range.
func DiagramRules(eff config.Effective, lang string) string {
	if !eff.Diagram {
		return ""
	}
	return `

Diagram ("diagram" field):
- Fill "diagram" with a small Mermaid diagram of what this merge request IMPLEMENTS: the behavior end to end (who calls what, in which order, which data is read or written, which other services, queues, webhooks or events are involved), not the file structure. Prefer "sequenceDiagram" for interactions between components, services, the database, queues or external systems; use "flowchart TD" for control-flow or data changes.
- Draw it for every merge request that changes behavior, even a small one: a new or changed endpoint, handler, event, job, query or business rule.
- Return "" only for trivial changes with no behavior change: renames, formatting, typos, comments or docs, dependency or version bumps, tests only, a one-line local fix.
- Keep it small: at most 25 nodes/messages, at most ` + fmt.Sprint(maxDiagramLines) + ` lines, each line shorter than ` + fmt.Sprint(maxDiagramLineRunes) + ` characters. Write labels in ` + lang + `.
- Highlight what is new or changed: in a sequenceDiagram with "Note over X: ..." notes; in a flowchart with "classDef changed fill:#fff3bf,stroke:#f08c00" and the ":::changed" class on new/changed nodes.
- Raw Mermaid only: the first line must be "sequenceDiagram" or "flowchart TD" (or another direction). No code fences, no "%%{init}" directives, no click/link/href/call statements, no HTML tags (no <br>), no URLs. A diagram that breaks these rules is dropped.
- In an INCREMENTAL review (the user prompt says whether this one is full or incremental) the summary keeps the previous diagram when you return "". Return a new diagram only if the new commits change the picture, and then draw the merge request as a whole.`
}

// DiagramSchema keeps the optional "diagram" property of the review schema
// when on and removes it when the diagram is disabled.
func DiagramSchema(schema string, on bool) string {
	if on {
		return schema
	}
	out, err := jsonschema.EditProps(schema, func(props map[string]any) bool {
		if _, ok := props["diagram"]; !ok {
			return false
		}
		delete(props, "diagram")
		return true
	})
	switch {
	case errors.Is(err, jsonschema.ErrNoProps):
		return schema // nothing to remove
	case err != nil:
		panic(err.Error())
	}
	return out
}
