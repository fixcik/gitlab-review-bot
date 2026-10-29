package linters

import (
	"cmp"
	"fmt"
	"sort"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// RenderCIBlock renders the "🔧 Линтеры из CI" block with markers ("" when
// lint.ci is not configured). Job names, rules, paths and messages come
// from the MR's pipeline: the content is defused (summary.Block), only the
// pending marker is the bot's.
func RenderCIBlock(lr *Run) string {
	if lr == nil || lr.ci == nil {
		return ""
	}
	c := lr.ci
	var b strings.Builder
	var waiting []string
	for _, e := range c.entries {
		if e.st.State == lint.StatePending {
			waiting = append(waiting, e.waitingJobs())
		}
	}
	if len(waiting) > 0 {
		b.WriteString("**" + CITitle + ":** ⏳ ждём job " + strings.Join(waiting, ", ") +
			" — замечания появятся тредами после завершения пайплайна, без нового ревью.\n")
	}
	if done := len(c.entries) - len(waiting); done > 0 {
		if len(waiting) > 0 {
			b.WriteString("\n")
		}
		renderCILintDetails(&b, lr)
	}
	pend := c.pending() // configured tool names (config: [a-z0-9._-])
	sort.Strings(pend)
	return summary.Block(marker.CILintStart+"\n", b.String(),
		"<!-- reviewbot:cilint-pending "+strings.Join(pend, ",")+" -->\n"+marker.CILintEnd)
}

// renderCILintDetails renders the collapsed results of the finished entries.
func renderCILintDetails(b *strings.Builder, lr *Run) {
	c := lr.ci
	found, bad := len(c.findings), 0
	for _, e := range c.entries {
		if e.st.State == lint.StateUnavailable || e.st.State == lint.StateFailed {
			bad++
		}
	}
	head := fmt.Sprintf("%s: %d %s на изменённых строках", CITitle, found, mdtext.Plural(found, "замечание", "замечания", "замечаний"))
	if bad > 0 {
		head += fmt.Sprintf(", нет отчёта: %d", bad)
	}
	fmt.Fprintf(b, "<details>\n<summary>%s</summary>\n\n", head)
	b.WriteString("| Линтер | Job | Результат |\n|---|---|---|\n")
	for _, e := range c.entries {
		fmt.Fprintf(b, "| %s | %s | %s |\n", e.st.Tool, mdtext.TableCell(e.jobsCell()), mdtext.TableCell(ciCell(e.st)))
	}
	b.WriteString("\n")
	var stats []string
	if n := countCI(lr.Published); n > 0 {
		stats = append(stats, fmt.Sprintf("новых тредов: %d", n))
	}
	if lr.skippedCI > 0 {
		stats = append(stats, fmt.Sprintf("уже обсуждаются: %d", lr.skippedCI))
	}
	if lr.closedCI > 0 {
		stats = append(stats, fmt.Sprintf("закрыто как исправленные: %d", lr.closedCI))
	}
	if len(stats) > 0 {
		b.WriteString(strings.Join(stats, ", ") + ".\n\n")
	}
	if notes := ciLintNotes(lr); len(notes) > 0 {
		b.WriteString(strings.Join(notes, "\n") + "\n\n")
	}
	renderCIOutside(b, lr.Outside)
	if p := c.pipeline; p != nil && p.ID > 0 {
		b.WriteString("<sub>Отчёты из пайплайна " + cijob.PipelineLink(*p) + "; линтеры запускает CI, бот только читает отчёты.</sub>\n")
	}
	b.WriteString("</details>\n")
}

// ciLintNotes explains the entries without a full report.
func ciLintNotes(lr *Run) []string {
	var notes []string
	for _, e := range lr.ci.entries {
		switch e.st.State {
		case lint.StateUnavailable, lint.StateFailed:
			// Reasons are built here (names through mdtext.InlineCode, parser
			// errors through mdtext.Inline).
			notes = append(notes, fmt.Sprintf("⚠️ %s: %s.", e.st.Tool, mdtext.OneLine(cmp.Or(e.st.Reason, "неизвестная ошибка"))))
		case lint.StatePartial:
			notes = append(notes, fmt.Sprintf("ℹ️ %s: %s. Открытые треды этого линтера не закрываются автоматически.", e.st.Tool, mdtext.OneLine(e.st.Reason)))
		}
	}
	if lr.err != nil && lr.Res != nil {
		notes = append(notes, "⚠️ Не удалось прочитать треды MR: результаты не опубликованы тредами и ничего не закрыто.")
	}
	return notes
}

// renderCIOutside lists the CI results that could not be posted as threads.
func renderCIOutside(b *strings.Builder, outside []lint.Finding) {
	var out []lint.Finding
	for _, f := range outside {
		if lint.IsCI(f.Tool) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return
	}
	sort.SliceStable(out, func(i, j int) bool { return finding.Rank(out[i].Severity) < finding.Rank(out[j].Severity) })
	b.WriteString("Не опубликованы тредами (строка не попала в дифф или ошибка GitLab):\n")
	for i, f := range out {
		if i == ciMaxOutside {
			fmt.Fprintf(b, "- … и ещё %d\n", len(out)-ciMaxOutside)
			break
		}
		fmt.Fprintf(b, "- **%s** · %s %s · %s — %s\n", finding.Badge(f.Severity), f.Tool,
			mdtext.Code(f.Rule), mdtext.Code(mdtext.PathLine(f.Path, f.Line)), mdtext.Inline(f.Message, 200))
	}
	b.WriteString("\n")
}

func countCI(fs []lint.Finding) int {
	n := 0
	for _, f := range fs {
		if lint.IsCI(f.Tool) {
			n++
		}
	}
	return n
}

// waitingJobs names the unfinished jobs of a pending entry.
func (e ciEntry) waitingJobs() string {
	names := e.waiting()
	if len(names) == 0 {
		return mdtext.InlineCode(e.cfg.Job)
	}
	return strings.Join(names, ", ")
}

// jobsCell links the matched jobs (or names the configured job).
func (e ciEntry) jobsCell() string {
	if len(e.jobs) == 0 {
		return mdtext.InlineCode(e.cfg.Job)
	}
	var out []string
	for i, j := range e.jobs {
		if i == 3 {
			out = append(out, fmt.Sprintf("и ещё %d", len(e.jobs)-3))
			break
		}
		if mdtext.IsHTTPURL(j.WebURL) {
			out = append(out, "["+strings.NewReplacer("[", "", "]", "", "|", "").Replace(mdtext.OneLine(j.Name))+"]("+j.WebURL+")")
		} else {
			out = append(out, mdtext.InlineCode(j.Name))
		}
	}
	return strings.Join(out, ", ")
}

func ciCell(st lint.ToolStatus) string {
	switch st.State {
	case lint.StatePending:
		return "⏳ ждём завершения job"
	case lint.StateUnavailable, lint.StateFailed:
		return "⚠️ нет отчёта"
	}
	s := "✅ замечаний нет"
	if st.Found > 0 {
		s = fmt.Sprintf("%d на изменённых строках", st.Found)
	}
	if st.Total > st.Found {
		s += fmt.Sprintf(" (всего в отчёте: %d)", st.Total)
	}
	if st.State == lint.StatePartial {
		s += ", частично"
	}
	return s
}

// CISection is the block as a summary section ("" without lint.ci).
func CISection(lr *Run) string {
	if blk := RenderCIBlock(lr); blk != "" {
		return blk + "\n\n"
	}
	return ""
}

// ReplaceCIBlock swaps the block in a summary body; without a block it goes
// after the lint section, else at the end.
func ReplaceCIBlock(body, block string) string {
	return summary.SwapBlock(body, block, marker.CILintRe)
}
