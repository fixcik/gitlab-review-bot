// Package summary is the bot's summary note as a document: rendering it,
// its footer and merge-gate line, and editing an existing summary body (status
// lines, marker-delimited blocks and where new blocks go). It is a leaf: it
// must not depend on internal/review or any of its feature packages.
package summary

import (
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// Title is the heading line of the summary note.
const Title = "## 🤖 Ревью"

// Data is what Render puts into a summary note.
type Data struct {
	HeadSHA     string
	From, To    string
	Incremental bool
	Output      *output.ReviewOutput
	Published   []output.Finding // findings posted as threads (new this run)
	Outside     []output.Finding // findings that could not be anchored
	Skipped     int              // duplicates of open threads
	Closed      int
	Partial     *Partial
	Mention     string
	TitleHint   string // marker-delimited block, see title.go
	TaskSection string // "📋 Соответствие задаче", see task.go
	// Gate: show the merge-gate line; BlockingOpen is the number of open
	// merge-blocking threads after this run.
	Gate         bool
	BlockingOpen int
	// FixedPending (part of BlockingOpen): blocking threads the bot marked
	// fixed but could not resolve (no service token); they still block in
	// GitLab until a human closes them.
	FixedPending int
	// CompatSection is "🧩 Обратная совместимость" (see compatcheck/section.go), placed
	// right after the gate line.
	CompatSection string
	// Route is the model route for the footer ("" = none, route.go).
	Route string
}

// Partial says how much of a too large change was reviewed.
type Partial struct {
	Reviewed, Total int
	Omitted         []string
	Truncated       bool
}

// Render renders the summary note of a review. Only the bot's markers may
// open an HTML comment in a summary: its blocks are found again with regular
// expressions over the whole body. So everything Render writes itself (the
// model's text, paths, titles, the footer) goes through one choke point,
// own (mdtext.DefuseComments); the sections it gets ready-made (TitleHint,
// CompatSection, TaskSection) defuse their own text, their blocks through
// Block.
func Render(d Data) string {
	var b strings.Builder
	b.WriteString(marker.Summary(d.HeadSHA))
	b.WriteString("\n")
	b.WriteString(Title)
	b.WriteString("\n\n")
	if d.TitleHint != "" {
		b.WriteString(d.TitleHint)
		b.WriteString("\n\n")
	}
	if d.Gate {
		b.WriteString(GateLine(d.BlockingOpen, d.FixedPending))
		b.WriteString("\n\n")
	}
	b.WriteString(d.CompatSection)
	b.WriteString(own(summaryText(d.Output.Summary)))
	b.WriteString(d.TaskSection)
	b.WriteString(own(renderTail(d)))
	return mdtext.NeutralizeQuickActions(b.String())
}

// own defuses the text Render writes itself (it has no bot markers).
func own(s string) string { return mdtext.DefuseComments(s) }

func summaryText(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return s + "\n\n"
	}
	return ""
}

// renderTail is the summary after the task section: walkthrough,
// statistics, partial review, findings outside the diff and the footer.
func renderTail(d Data) string {
	var b strings.Builder
	if len(d.Output.Walkthrough) > 0 {
		fmt.Fprintf(&b, "<details>\n<summary>Обзор изменений (%d)</summary>\n\n", len(d.Output.Walkthrough))
		b.WriteString("| Файл | Изменения |\n|---|---|\n")
		for _, w := range d.Output.Walkthrough {
			fmt.Fprintf(&b, "| %s | %s |\n", mdtext.CodeCell(w.Path), mdtext.TableCell(w.Summary))
		}
		b.WriteString("\n</details>\n\n")
	}

	all := append(append([]output.Finding{}, d.Published...), d.Outside...)
	counts := map[core.Severity]int{}
	for _, f := range all {
		counts[f.Severity]++
	}
	var parts []string
	for _, s := range finding.AllSeverities {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", finding.Badge(s), counts[s]))
		}
	}
	fmt.Fprintf(&b, "**Статистика:** найдено %d", len(all))
	if len(parts) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
	}
	fmt.Fprintf(&b, ", закрыто %d", d.Closed)
	if d.Skipped > 0 {
		fmt.Fprintf(&b, ", уже обсуждаются %d", d.Skipped)
	}
	b.WriteString(".\n\n")

	if p := d.Partial; p != nil {
		if p.Reviewed < p.Total {
			fmt.Fprintf(&b, "⚠️ Изменения слишком большие: ревью проведено по %d из %d файлов.", p.Reviewed, p.Total)
			if len(p.Omitted) > 0 {
				b.WriteString(" Не просмотрены: ")
				const maxList = 20
				names := make([]string, 0, maxList)
				for i, f := range p.Omitted {
					if i == maxList {
						names = append(names, fmt.Sprintf("и ещё %d", len(p.Omitted)-maxList))
						break
					}
					names = append(names, mdtext.Code(f))
				}
				b.WriteString(strings.Join(names, ", "))
				b.WriteString(".")
			}
			b.WriteString("\n\n")
		} else if p.Truncated {
			b.WriteString("⚠️ Дифф слишком большой и был обрезан — часть изменений могла остаться без внимания.\n\n")
		}
	}

	if len(d.Outside) > 0 {
		b.WriteString("### Замечания вне диффа\n\n")
		for _, f := range d.Outside {
			fmt.Fprintf(&b, "- **%s** · %s — %s\n", finding.Badge(f.Severity), mdtext.CodeLoc(f.Path, f.Line), mdtext.OneLine(f.Title))
			if f.Body != "" {
				b.WriteString(mdtext.Indent(f.Body, "  "))
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}

	b.WriteString(WithRouteFooter(Footer(d.From, d.To, d.Incremental, d.Mention), d.Route))
	return b.String()
}

// Block puts the content of a summary block between its start and end
// markers. Every block of the summary is built with it: the content (paths,
// job names, the model's and the tools' text) is defused
// (mdtext.DefuseComments), so that it can hold no marker the regular
// expressions that find the blocks would take for the bot's own. A marker
// the bot itself writes inside a block (the CI linters' pending list, the
// stored title suggestion) goes with start or end.
func Block(start, content, end string) string {
	return start + mdtext.DefuseComments(content) + end
}

// Footer is the summary footer: the reviewed range and the commands.
func Footer(from, to string, incremental bool, mention string) string {
	var b strings.Builder
	b.WriteString("---\n")
	if from != "" && to != "" {
		mode := "полное"
		if incremental {
			mode = "инкрементальное"
		}
		fmt.Fprintf(&b, "<sub>Проверено: `%s..%s` (%s)</sub>\n\n", mdtext.ShortSHA(from), mdtext.ShortSHA(to), mode)
	}
	fmt.Fprintf(&b, "<sub>Команды: %s</sub>\n", CommandsInline(mention))
	return b.String()
}

// WithRouteFooter puts the route into the "Проверено" line of a summary
// footer (Footer).
func WithRouteFooter(footer, route string) string {
	if route == "" {
		return footer
	}
	if i := strings.Index(footer, "</sub>\n"); i >= 0 && strings.Contains(footer[:i], "Проверено") {
		return footer[:i] + " · " + route + footer[i:]
	}
	return strings.Replace(footer, "---\n", "---\n<sub>"+route+"</sub>\n\n", 1)
}

// CommandsInline lists the commands on one line, addressed to mention m.
func CommandsInline(m string) string {
	return fmt.Sprintf("`%s review` — ревью новых коммитов · `%s full review` — полное ревью · `%s help` — справка · `%s <вопрос>` — вопрос по коду", m, m, m, m)
}

// CommandsList lists the commands as a Markdown list (help).
// The lines are core.KindInfo.Help.
func CommandsList(m string) string {
	return core.JobReview.Help(m) + core.JobFullReview.Help(m) + core.JobForceReview.Help(m) +
		core.JobHelp.Help(m) + core.JobChat.Help(m)
}
