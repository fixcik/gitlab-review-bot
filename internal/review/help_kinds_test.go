package review_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// The "@bot help" command list is built from core.KindInfo.Help; these are
// the texts (and the order) the bot answered before the kind table, and the
// answer must stay byte-identical.
var (
	helpBefore = "- `%[1]s review` (`%[1]s ревью`) — ревью новых коммитов с прошлого ревью\n" +
		"- `%[1]s full review` (`%[1]s полное ревью`) — полное ревью MR заново\n" +
		"- `%[1]s review force` (`%[1]s ревью принудительно`) — полное ревью, даже если MR больше порога `review.max_mr_lines`\n" +
		"- `%[1]s help` (`%[1]s помощь`) — эта справка\n" +
		"- `%[1]s <вопрос>` — вопрос по коду; в треде замечания бот учитывает обсуждение и может закрыть тред\n"
	helpRulesBefore = "- `%[1]s запомни <правило>` (`remember`) — запомнить правило команды; область (этот репозиторий или все) выберет бот, либо допишите «для всех» / «только здесь»\n" +
		"- `%[1]s правила` (`rules`) — правила, которые действуют в этом репозитории\n" +
		"- `%[1]s забудь #N` (`forget #N`) — удалить правило\n" +
		"- `%[1]s правило #N для всех` / `только здесь` (`rule #N global|local`) — изменить область правила\n" +
		"- Ответ Developer+ в треде замечания бота с объяснением, почему оно неверно, бот запоминает как правило\n"
	helpMRBefore = "- `%[1]s pause` (`%[1]s пауза`) — остановить автоматическое ревью этого MR (ручное ревью, вопросы и правила работают)\n" +
		"- `%[1]s resume` (`%[1]s продолжить`) — снова включить авторевью; непроверенные коммиты будут проверены\n" +
		"- `%[1]s check` (`%[1]s перепроверь`) — перепроверить открытые замечания бота на текущем коде без нового коммита и закрыть исправленные; в треде замечания — только его\n" +
		"- `%[1]s resolve` (`%[1]s закрой треды`) — закрыть все открытые треды бота в этом MR, включая блокирующие (нужен служебный токен)\n" +
		"- `%[1]s config` (`%[1]s конфиг`) — действующие настройки бота для этого MR и откуда они взяты\n" +
		"- `%[1]s ignore` (`%[1]s игнорировать`) **в описании MR** — не запускать авторевью, пока строка есть в описании\n"
	helpMemoryBefore = "- `%[1]s память` (`memory`) — память проекта: что бот выяснил о сборке, тестах, линте и структуре этого репозитория\n" +
		"- `%[1]s память очистить [раздел]` (`memory clear [section]`) — очистить раздел или всю память\n" +
		"- `%[1]s память запомни <раздел>: <текст>` (`memory add <section>: <text>`) — добавить пункт вручную (разделы: сборка, тесты, линт, подготовка окружения, структура проекта, подводные камни, прочее)\n"
)

func TestHelpCommandListUnchanged(t *testing.T) {
	const m = "@reviewbot"
	header := "### 🤖 Команды бота\n\n"
	tail := "\nАвтоматическое ревью запускается при открытии MR"

	// learning on, memory off
	h := reviewtest.NewLearnHarness(t, nil)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	want := header + fmt.Sprintf(helpBefore+helpRulesBefore+helpMRBefore, m) + tail
	if body := reviewtest.LastReply(t, h.GL).Body; !strings.HasPrefix(body, want) {
		t.Errorf("help with rules:\n got %q\nwant prefix %q", body, want)
	}

	// memory on
	hm := reviewtest.NewMemHarness(t)
	if err := hm.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	want = fmt.Sprintf(helpMRBefore+helpMemoryBefore, m) + tail
	if body := reviewtest.LastReply(t, hm.GL).Body; !strings.Contains(body, want) {
		t.Errorf("help with memory:\n got %q\nwant it to contain %q", body, want)
	}

	// both off
	h0 := reviewtest.New(t)
	if err := h0.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	want = header + fmt.Sprintf(helpBefore+helpMRBefore, m) + tail
	if body := reviewtest.LastReply(t, h0.GL).Body; !strings.HasPrefix(body, want) {
		t.Errorf("help:\n got %q\nwant prefix %q", body, want)
	}
}

// Every kind with a help text is listed in the full help (a kind added to
// the table with Help must also be added to one of the help groups).
func TestHelpListsEveryKindWithHelp(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil) // rules on
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	help := reviewtest.LastReply(t, h.GL).Body + reviewtest.NewMemHarness(t).Internals().Learning.MemoryCommandsList()
	for _, k := range core.AllJobKinds() {
		if s := k.Help("@reviewbot"); s != "" && !strings.Contains(help, s) {
			t.Errorf("%s: its help is not in @bot help", k)
		}
	}
}
