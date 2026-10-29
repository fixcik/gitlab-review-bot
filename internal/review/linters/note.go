package linters

import (
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

const (
	lintBadge   = "🔧"
	secretBadge = "🔑"
	// secretAdvice is the one thing a secret thread must say.
	secretAdvice = "секрет нужно отозвать и удалить из истории"
)

// NoteBody renders the first note of a lint thread. For gitleaks it
// contains only the rule id, the location and the commit: never the value.
func NoteBody(id string, f lint.Finding, blocking, nonBlocking, outsideDiff bool) string {
	if f.Todo != nil {
		return todoNoteBody(id, f, blocking, nonBlocking, outsideDiff) // linters/todo.go
	}
	var b strings.Builder
	b.WriteString(marker.Lint(id, f.Tool))
	b.WriteString("\n")
	if blocking {
		b.WriteString(marker.Blocking)
		b.WriteString("\n")
	}
	rule := mdtext.Code(f.Rule)
	badge := lintBadge + " " + f.Tool
	if f.Secret() {
		badge = secretBadge + " " + badge
	}
	fmt.Fprintf(&b, "**%s** · %s · %s\n", badge, rule, finding.Badge(f.Severity))
	loc := mdtext.FileLineRU(f.Path, f.Line)
	if f.Secret() {
		b.WriteString("\n⛔ **Блокирует мерж: в изменениях найден секрет.** Его " + secretAdvice + ": удаления в новом коммите недостаточно, значение остаётся в истории git. Отзовите (смените) секрет, перепишите историю ветки без него и закройте тред.\n")
		where := "\n_Файл " + loc
		if f.Commit != "" {
			where += ", коммит " + mdtext.Code(f.Commit)
		}
		if !f.InDiff {
			where += " (в истории коммитов MR, не в итоговом диффе)"
		}
		b.WriteString(where + "._\n")
		// No tool text at all: only the rule id, the location and the advice.
		b.WriteString("\n_Значение секрета намеренно не показывается._\n")
		return mdtext.NeutralizeQuickActions(b.String())
	}
	switch {
	case blocking:
		b.WriteString("\n⛔ **Блокирует мерж.** Исправьте и закройте тред.\n")
	case nonBlocking:
		b.WriteString("\n_Не блокирует мерж._\n")
	}
	if outsideDiff {
		fmt.Fprintf(&b, "\n_Файл %s (вне диффа)._\n", loc)
	}
	if f.Message != "" {
		b.WriteString("\n" + mdtext.Inline(f.Message, 600) + "\n")
	}
	if f.URL != "" {
		b.WriteString("\n[Описание правила](" + f.URL + ")\n")
	}
	b.WriteString("\n_Результат " + f.Tool + " (автоматическая проверка, без участия Claude)._\n")
	return mdtext.NeutralizeQuickActions(b.String())
}
