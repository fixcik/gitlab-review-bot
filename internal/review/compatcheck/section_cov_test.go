package compatcheck

import (
	"errors"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
)

// richRun has every part of the section.
func richRun() *Run {
	claude := Item{ID: "c1", Source: SourceClaude, Path: "db/m.sql", Line: 4, Title: "Удалена *колонка* email",
		PreviousBehavior: "колонка\nесть", NewBehavior: "", SafeAlternative: "сначала перестать читать", Unverified: true, VerifyNote: "достигнут общий лимит запусков Claude"}
	tool := Item{ID: "t1", Source: compat.SourceOasdiff, Path: "api.yaml", Line: 7, Title: "GET /u: removed `email` <b>@team</b>"}
	return &Run{Enabled: true, Items: []Item{claude, tool, claude},
		Remaining: []mrthread.OpenCompat{{Source: compat.SourceBuf, Path: "a.proto", Line: 2, Title: "field *x* deleted", Body: ""}},
		dismissed: []Item{{ID: "d1", Source: compat.SourceBuf, Path: "b.proto", Title: "enum `Y` removed"}},
		Outside:   []Item{tool, tool},
		Closed:    2,
		Err:       errors.New("x"),
		Res: &compat.Result{
			Issues: []compat.Issue{
				{Source: compat.SourceBuf, Path: "p/`x`.proto", Kind: compat.IssueUnavailable, Reason: "нет buf"},
				{Source: compat.SourceGraphQL, Kind: compat.IssueFailed, Reason: "exit *1*"},
				{Source: compat.SourceGraphQL, Kind: compat.IssueFailed, Reason: "exit *1*"},
				{Source: compat.SourceOasdiff, Path: "s.yaml", Kind: compat.IssueSkipped, Reason: "новый файл"},
				{Source: compat.SourceOasdiff, Kind: "other", Reason: "ignored"},
			},
			Warnings: []compat.Change{{Source: compat.SourceOasdiff, Path: "w.yaml", Line: 3, Message: "maybe <i>"}},
		},
		rejected: []Rejected{{Item: Item{Path: "a.go", Line: 1, Title: "t1\nx"}, Reason: "алиас\nостался"}, {Item: Item{Path: "b.go", Title: "t2"}}},
	}
}

// The whole section: blocking items (Claude's unverified one with its
// details, a tool item escaped, an earlier open thread) listed once each,
// dismissed threads apart, threads that could not be created, fixed
// threads, a failed run, tool issues sorted and de-duplicated (unknown kinds
// ignored), unconfirmed warnings and rejected claims collapsed.
func TestRenderSectionGolden(t *testing.T) {
	want := "### 🧩 Обратная совместимость\n" +
		"\n" +
		"⛔ **Найдено 3 несовместимых изменения** — мерж заблокирован до исправления:\n" +
		"\n" +
		"- **анализ Claude** · `db/m.sql:4` — Удалена *колонка* email _(не перепроверено: достигнут общий лимит запусков Claude)_\n" +
		"  - Было: колонка есть → Стало: —\n" +
		"  - Безопасная альтернатива: сначала перестать читать\n" +
		"- **oasdiff** · `api.yaml:7` — GET /u: removed \\`email\\` \\<b\\>@​team\\</b\\>\n" +
		"- **buf** · `a.proto:2` — field *x* deleted _(найдено ранее, тред открыт)_\n" +
		"\n" +
		"Тред закрыт вручную (изменение принято осознанно, мерж не блокирует):\n" +
		"- **buf** · `b.proto` — enum \\`Y\\` removed\n" +
		"\n" +
		"⚠️ Не удалось создать 2 треда для несовместимых изменений — проверьте список выше вручную.\n" +
		"\n" +
		"Исправлено с прошлого ревью: 2.\n" +
		"\n" +
		"⚠️ Автоматическая проверка спецификаций не выполнена (внутренняя ошибка, подробности в логах бота).\n" +
		"\n" +
		"ℹ️ oasdiff (`s.yaml`): новый файл.\n" +
		"⚠️ Проверка buf недоступна (`p/x.proto`): нет buf.\n" +
		"⚠️ Проверка graphql-inspector не выполнена: exit \\*1\\*.\n" +
		"\n" +
		"<details>\n" +
		"<summary>Возможные несовместимости, которые инструменты не смогли подтвердить (1, не блокируют)</summary>\n" +
		"\n" +
		"- **oasdiff** · `w.yaml` — maybe \\<i\\>\n" +
		"\n" +
		"</details>\n" +
		"\n" +
		"<details>\n" +
		"<summary>Отклонено при перепроверке (2)</summary>\n" +
		"\n" +
		"- `a.go:1` — t1 x — алиас остался\n" +
		"- `b.go` — t2\n" +
		"\n" +
		"</details>\n" +
		"\n"
	if got := RenderSection(richRun()); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Nothing to report: the one line, nothing else.
	if got := RenderSection(&Run{Enabled: true, Res: &compat.Result{}}); got != "### 🧩 Обратная совместимость\n\n✅ Несовместимых изменений не найдено.\n\n" {
		t.Errorf("empty section %q", got)
	}
	if RenderSection(nil) != "" || RenderSection(&Run{}) != "" {
		t.Error("check off: no section")
	}
	if strings.Count(RenderSection(richRun()), "Удалена") != 1 {
		t.Error("an item listed twice")
	}
}
