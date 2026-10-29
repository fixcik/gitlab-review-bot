package compatcheck

// The "🧩 Обратная совместимость" section of the summary note.

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// RenderSection renders "🧩 Обратная совместимость" ("" when the check
// is off).
func RenderSection(cr *Run) string {
	if cr == nil || !cr.Enabled {
		return ""
	}
	var b strings.Builder
	b.WriteString(summary.CompatSectionTitle + "\n\n")
	writeCompatList(&b, cr)
	if len(cr.dismissed) > 0 {
		b.WriteString("Тред закрыт вручную (изменение принято осознанно, мерж не блокирует):\n")
		for _, it := range cr.dismissed {
			fmt.Fprintf(&b, "- **%s** · %s — %s\n", sourceName(it.Source), mdtext.CodeLoc(it.Path, 0), compatListTitle(it, false))
		}
		b.WriteString("\n")
	}
	if len(cr.Outside) > 0 {
		fmt.Fprintf(&b, "⚠️ Не удалось создать %d %s для несовместимых изменений — проверьте список выше вручную.\n\n",
			len(cr.Outside), mdtext.Plural(len(cr.Outside), "тред", "треда", "тредов"))
	}
	if cr.Closed > 0 {
		fmt.Fprintf(&b, "Исправлено с прошлого ревью: %d.\n\n", cr.Closed)
	}
	if cr.Err != nil {
		b.WriteString("⚠️ Автоматическая проверка спецификаций не выполнена (внутренняя ошибка, подробности в логах бота).\n\n")
	}
	if cr.Res != nil {
		writeCompatIssues(&b, cr.Res)
	}
	if len(cr.rejected) > 0 {
		fmt.Fprintf(&b, "<details>\n<summary>Отклонено при перепроверке (%d)</summary>\n\n", len(cr.rejected))
		for _, rj := range cr.rejected {
			fmt.Fprintf(&b, "- %s — %s", mdtext.CodeLoc(rj.Item.Path, rj.Item.Line), mdtext.OneLine(rj.Item.Title))
			if rj.Reason != "" {
				b.WriteString(" — " + mdtext.OneLine(rj.Reason))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n</details>\n\n")
	}
	// The section goes into the summary: no "<!--" from the MR (summary.Block).
	return mdtext.DefuseComments(b.String())
}

// writeCompatList lists the breaking changes that block the merge: this
// run's items and the still open threads of earlier runs (manually resolved
// ones are listed separately).
func writeCompatList(b *strings.Builder, cr *Run) {
	type line struct {
		it      Item
		earlier bool
	}
	var list []line
	listed := map[string]bool{}
	for _, it := range cr.dismissed {
		listed[it.ID] = true
	}
	for _, it := range cr.Items {
		if listed[it.ID] {
			continue
		}
		listed[it.ID] = true
		list = append(list, line{it: it})
	}
	for _, oc := range cr.Remaining {
		id, src := openCompatID(oc)
		if listed[id] {
			continue
		}
		listed[id] = true
		list = append(list, line{it: Item{ID: id, Source: src, Path: oc.Path, Line: oc.Line, Title: oc.Title}, earlier: true})
	}
	if len(list) == 0 {
		b.WriteString("✅ Несовместимых изменений не найдено.\n\n")
		return
	}
	n := len(list)
	fmt.Fprintf(b, "⛔ **Найдено %d %s** — мерж заблокирован до исправления:\n\n", n,
		mdtext.Plural(n, "несовместимое изменение", "несовместимых изменения", "несовместимых изменений"))
	for _, l := range list {
		it := l.it
		fmt.Fprintf(b, "- **%s** · %s — %s", sourceName(it.Source), mdtext.CodeLoc(it.Path, it.Line), compatListTitle(it, l.earlier))
		switch {
		case l.earlier:
			b.WriteString(" _(найдено ранее, тред открыт)_")
		case it.Unverified:
			b.WriteString(" _(не перепроверено: " + it.VerifyNote + ")_")
		}
		b.WriteString("\n")
		if it.PreviousBehavior != "" || it.NewBehavior != "" {
			fmt.Fprintf(b, "  - Было: %s → Стало: %s\n", mdtext.OneLine(cmp.Or(it.PreviousBehavior, "—")), mdtext.OneLine(cmp.Or(it.NewBehavior, "—")))
		}
		if it.SafeAlternative != "" {
			b.WriteString("  - Безопасная альтернатива: " + mdtext.OneLine(it.SafeAlternative) + "\n")
		}
	}
	b.WriteString("\n")
}

// compatListTitle is an item's title for a summary line: Claude's text, or
// a title read back from our own note (earlier: already escaped when it was
// written), as one line; tool text escaped.
func compatListTitle(it Item, earlier bool) string {
	if it.Source == SourceClaude || earlier {
		return mdtext.OneLine(it.Title)
	}
	return mdtext.Inline(it.Title, 300)
}

// writeCompatIssues writes the checks that could not run and the warnings
// the tools could not confirm.
func writeCompatIssues(b *strings.Builder, res *compat.Result) {
	var notes []string
	for _, is := range res.Issues {
		subject := ""
		if is.Path != "" {
			subject = " (`" + strings.ReplaceAll(mdtext.Shorten(is.Path, 120), "`", "") + "`)"
		}
		switch is.Kind {
		case compat.IssueUnavailable:
			notes = append(notes, fmt.Sprintf("⚠️ Проверка %s недоступна%s: %s.", is.Source, subject, mdtext.Inline(is.Reason, 200)))
		case compat.IssueFailed:
			notes = append(notes, fmt.Sprintf("⚠️ Проверка %s не выполнена%s: %s.", is.Source, subject, mdtext.Inline(is.Reason, 300)))
		case compat.IssueSkipped:
			notes = append(notes, fmt.Sprintf("ℹ️ %s%s: %s.", is.Source, subject, mdtext.Inline(is.Reason, 200)))
		}
	}
	sort.Strings(notes)
	for _, n := range slices.Compact(notes) {
		b.WriteString(n + "\n")
	}
	if len(notes) > 0 {
		b.WriteString("\n")
	}
	if len(res.Warnings) > 0 {
		fmt.Fprintf(b, "<details>\n<summary>Возможные несовместимости, которые инструменты не смогли подтвердить (%d, не блокируют)</summary>\n\n", len(res.Warnings))
		for _, w := range res.Warnings {
			fmt.Fprintf(b, "- **%s** · %s — %s\n", w.Source, mdtext.CodeLoc(w.Path, 0), mdtext.Inline(w.Message, 300))
		}
		b.WriteString("\n</details>\n\n")
	}
}
