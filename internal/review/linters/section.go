package linters

import (
	"cmp"
	"fmt"
	"sort"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

const (
	// SectionTitle heads the "🔧 Линтеры и сканеры" summary block.
	SectionTitle = "🔧 Линтеры и сканеры"
	// SecretSectionTitle heads the "🔑 Найдены секреты" summary block.
	SecretSectionTitle = "### 🔑 Найдены секреты"
)

// RenderSecrets is the "🔑 Найдены секреты: N" block shown right after
// the merge-gate line ("" when gitleaks found nothing).
func RenderSecrets(lr *Run) string {
	if lr == nil || lr.Res == nil {
		return ""
	}
	var secrets []lint.Finding
	for _, f := range lr.Res.Findings {
		if f.Secret() {
			secrets = append(secrets, f)
		}
	}
	if len(secrets) == 0 {
		return ""
	}
	dismissed := map[string]bool{}
	for _, f := range lr.dismissed {
		dismissed[f.ID()] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d\n\n", SecretSectionTitle, len(secrets))
	b.WriteString("⛔ Каждый секрет блокирует мерж. Его " + secretAdvice + " (удаления в новом коммите недостаточно). Значения не показываются.\n\n")
	for _, f := range secrets {
		fmt.Fprintf(&b, "- %s — %s", mdtext.Code(mdtext.PathLine(f.Path, f.Line)), mdtext.Code(f.Rule))
		if f.Commit != "" {
			fmt.Fprintf(&b, " (коммит %s)", mdtext.Code(f.Commit))
		}
		if !f.InDiff {
			b.WriteString(" _(только в истории коммитов MR)_")
		}
		if dismissed[f.ID()] {
			b.WriteString(" _(тред закрыт вручную — мерж не блокирует)_")
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	// The section goes into the summary: no "<!--" from the MR (summary.Block).
	return mdtext.DefuseComments(b.String())
}

// RenderSection is the collapsed "🔧 Линтеры и сканеры" block: per-tool
// counts, unavailable tools, results that could not be posted.
func RenderSection(lr *Run) string {
	if lr == nil {
		return ""
	}
	if lr.Res == nil {
		return "⚠️ " + SectionTitle + ": проверки не выполнены (внутренняя ошибка, подробности в логах бота).\n\n"
	}
	// CI linters have their own block (RenderCIBlock).
	res := &lint.Result{Notes: lr.Res.Notes}
	for _, st := range lr.Res.Tools {
		if !lint.IsCI(st.Tool) {
			res.Tools = append(res.Tools, st)
		}
	}
	for _, f := range lr.Res.Findings {
		if !lint.IsCI(f.Tool) {
			res.Findings = append(res.Findings, f)
		}
	}
	if len(res.Tools) == 0 && len(res.Findings) == 0 && len(res.Notes) == 0 && lr.err == nil && !lr.Cfg.SecretsKept {
		return ""
	}
	var b strings.Builder
	unavailable := 0
	for _, st := range res.Tools {
		if st.State == lint.StateUnavailable || st.State == lint.StateFailed {
			unavailable++
		}
	}
	head := fmt.Sprintf("%s: %d %s на изменённых строках", SectionTitle, len(res.Findings),
		mdtext.Plural(len(res.Findings), "замечание", "замечания", "замечаний"))
	if unavailable > 0 {
		head += fmt.Sprintf(", недоступно проверок: %d", unavailable)
	}
	fmt.Fprintf(&b, "<details>\n<summary>%s</summary>\n\n", head)
	b.WriteString("| Инструмент | Результат |\n|---|---|\n")
	found := map[string]int{}
	for _, f := range res.Findings {
		found[f.Tool]++
	}
	for _, st := range res.Tools {
		st.Found = found[st.Tool]
		fmt.Fprintf(&b, "| %s | %s |\n", st.Tool, mdtext.TableCell(lintToolCell(st)))
	}
	b.WriteString("\n")
	var stats []string
	if n := len(lr.Published) - countCI(lr.Published); n > 0 {
		stats = append(stats, fmt.Sprintf("новых тредов: %d", n))
	}
	if lr.skipped > 0 {
		stats = append(stats, fmt.Sprintf("уже обсуждаются: %d", lr.skipped))
	}
	if lr.Closed > 0 {
		stats = append(stats, fmt.Sprintf("закрыто как исправленные: %d", lr.Closed))
	}
	if len(stats) > 0 {
		b.WriteString(strings.Join(stats, ", ") + ".\n\n")
	}
	var notes []string
	for _, st := range res.Tools {
		switch st.State {
		case lint.StateUnavailable, lint.StateFailed:
			notes = append(notes, fmt.Sprintf("⚠️ Проверка %s недоступна: %s.", st.Tool, mdtext.Inline(cmp.Or(st.Reason, "неизвестная ошибка"), 200)))
		case lint.StatePartial:
			notes = append(notes, fmt.Sprintf("ℹ️ Проверка %s выполнена частично: %s. Открытые треды этой проверки не закрываются автоматически.", st.Tool, mdtext.Inline(st.Reason, 200)))
		}
	}
	for _, n := range res.Notes {
		notes = append(notes, "ℹ️ "+n)
	}
	if lr.Cfg.SecretsKept {
		notes = append(notes, "ℹ️ `.reviewbot.yaml` отключает gitleaks, но сервис это не разрешает (`review.lint.allow_disable_secrets`): поиск секретов выполнен.")
	}
	if lr.err != nil {
		notes = append(notes, "⚠️ Не удалось прочитать треды MR: результаты не опубликованы тредами и ничего не закрыто.")
	}
	for _, n := range notes {
		b.WriteString(n + "\n")
	}
	if len(notes) > 0 {
		b.WriteString("\n")
	}
	var out []lint.Finding
	for _, f := range lr.Outside {
		if !lint.IsCI(f.Tool) {
			out = append(out, f)
		}
	}
	if len(out) > 0 {
		sort.SliceStable(out, func(i, j int) bool { return finding.Rank(out[i].Severity) < finding.Rank(out[j].Severity) })
		b.WriteString("Не опубликованы тредами (строка не попала в дифф или ошибка GitLab):\n")
		for i, f := range out {
			if i == 30 {
				fmt.Fprintf(&b, "- … и ещё %d\n", len(out)-30)
				break
			}
			msg := mdtext.Inline(f.Message, 200)
			if f.Secret() {
				msg = "секрет (значение скрыто)"
			}
			fmt.Fprintf(&b, "- **%s** · %s %s · %s — %s\n", finding.Badge(f.Severity), f.Tool,
				mdtext.Code(f.Rule), mdtext.Code(mdtext.PathLine(f.Path, f.Line)), msg)
		}
		b.WriteString("\n")
	}
	b.WriteString("</details>\n\n")
	// The section goes into the summary: no "<!--" from the MR (summary.Block).
	return mdtext.DefuseComments(b.String())
}

func lintToolCell(st lint.ToolStatus) string {
	switch st.State {
	case lint.StateNotApplicable:
		return "— нечего проверять"
	case lint.StateUnavailable, lint.StateFailed:
		return "⚠️ недоступна"
	}
	var s string
	switch {
	case st.Found == 0 && st.Tool == lint.ToolGitleaks:
		s = "✅ секретов нет"
	case st.Found == 0:
		s = "✅ замечаний нет"
	case st.Tool == lint.ToolGitleaks:
		s = fmt.Sprintf("%s секретов: %d", secretBadge, st.Found)
	default:
		s = fmt.Sprintf("%d на изменённых строках", st.Found)
	}
	if st.Total > st.Found && st.Tool != lint.ToolGitleaks {
		s += fmt.Sprintf(" (всего в файлах: %d)", st.Total)
	}
	if st.State == lint.StatePartial {
		s += ", частично"
	}
	return s
}
