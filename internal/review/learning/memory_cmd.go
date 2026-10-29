package learning

// The memory commands: "@bot память", "@bot память очистить [раздел]",
// "@bot память запомни <раздел>: <текст>".

import (
	"context"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
)

const (
	MsgMemoryOff        = "Память проекта отключена в конфигурации бота (`review.memory.enabled`)."
	MsgMemoryRepoOff    = "Память проекта отключена для этого репозитория (`memory.enabled: false` в `.reviewbot.yaml`)."
	msgMemoryBadCommand = "Не понял команду. Доступно: `%[1]s память`, `%[1]s память очистить [раздел]`, `%[1]s память запомни <раздел>: <текст>`."
	msgMemoryBadSection = "Неизвестный раздел «%s». Разделы: %s."
	msgMemoryEmpty      = "Память проекта `%s` пока пуста. Бот заполняет её сам после ревью и ответов на вопросы, когда на выяснение чего-то (как собрать проект, запустить тесты или линт, где что лежит) ушло больше двух шагов. Добавить вручную: `%s память запомни тесты: <текст>`."
	msgMemoryCleared    = "🗑️ Память проекта очищена (разделов: %d)."
	msgMemoryClearedSec = "🗑️ Раздел «%s» памяти проекта очищен."
	msgMemoryNothing    = "Раздел «%s» и так пуст."
	msgMemoryAdded      = "📒 Запомнил в разделе «%s»: %s"
	msgMemoryKnown      = "📒 Это уже есть в разделе «%s»."
	msgMemoryNoText     = "Нечего запоминать: после удаления секретов текст пуст."
)

func sectionList() string {
	var ru, en []string
	for _, k := range memory.Sections {
		ru = append(ru, strings.ToLower(memory.Title(k)))
		en = append(en, k)
	}
	return strings.Join(ru, ", ") + " (или " + strings.Join(en, ", ") + ")"
}

// HandleMemory runs a memory command (no Claude run).
func (l *Feature) HandleMemory(ctx context.Context, job core.Job) error {
	if !l.memoryServiceOn() {
		return l.env.Reply(ctx, job, MsgMemoryOff)
	}
	mr, err := l.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return fmt.Errorf("get mr: %w", err)
	}
	eff, ok, err := l.env.RepoConfig.Command(ctx, job, mr)
	if !ok {
		return err
	}
	if !eff.MemoryEnabled {
		return l.env.Reply(ctx, job, MsgMemoryRepoOff)
	}
	if ok, err = l.env.CheckAccess(ctx, job); err != nil || !ok {
		return err
	}
	cmd, parsed := memory.ParseCommand(job.Question)
	if !parsed {
		return l.env.Reply(ctx, job, fmt.Sprintf(msgMemoryBadCommand, l.env.Mention()))
	}
	if cmd.BadSection != "" {
		return l.env.Reply(ctx, job, fmt.Sprintf(msgMemoryBadSection, cmd.BadSection, sectionList()))
	}
	switch cmd.Kind {
	case memory.CmdShow:
		return l.showMemory(ctx, job)
	case memory.CmdClear:
		n, err := l.memory.ClearMemory(ctx, job.ProjectID, cmd.Section)
		if err != nil {
			return err
		}
		l.env.Log.Info("memory: cleared", "project_id", job.ProjectID, "section", cmd.Section, "rows", n, "user", job.Username)
		switch {
		case cmd.Section == "":
			return l.env.Reply(ctx, job, fmt.Sprintf(msgMemoryCleared, n))
		case n == 0:
			return l.env.Reply(ctx, job, fmt.Sprintf(msgMemoryNothing, memory.Title(cmd.Section)))
		default:
			return l.env.Reply(ctx, job, fmt.Sprintf(msgMemoryClearedSec, memory.Title(cmd.Section)))
		}
	case memory.CmdAdd:
		text := memory.CleanBullet(cmd.Text)
		if text == "" {
			return l.env.Reply(ctx, job, msgMemoryNoText)
		}
		changes, err := l.mergeMemory(ctx, job, []memory.Update{{Section: cmd.Section, Op: memory.OpAdd, Text: text}},
			fmt.Sprintf("вручную @%s в !%d", job.Username, job.MRIID), false)
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			return l.env.Reply(ctx, job, fmt.Sprintf(msgMemoryKnown, memory.Title(cmd.Section)))
		}
		return l.env.Reply(ctx, job, fmt.Sprintf(msgMemoryAdded, memory.Title(cmd.Section), changes[len(changes)-1].Text))
	}
	return l.env.Reply(ctx, job, fmt.Sprintf(msgMemoryBadCommand, l.env.Mention()))
}

func (l *Feature) showMemory(ctx context.Context, job core.Job) error {
	secs, err := l.memory.ListMemory(ctx, job.ProjectID)
	if err != nil {
		return err
	}
	if len(secs) == 0 {
		return l.env.Reply(ctx, job, fmt.Sprintf(msgMemoryEmpty, job.ProjectPath, l.env.Mention()))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### 📒 Память проекта `%s`\n\n", job.ProjectPath)
	b.WriteString("Что бот выяснил о сборке, тестах и устройстве этого репозитория в прошлых запусках. Хранится в базе бота (не в репозитории) и подсказывает Claude в начале каждого запуска.\n\n")
	total := 0
	for _, s := range secs {
		total += len([]rune(s.Content))
		fmt.Fprintf(&b, "**%s** <sub>(`%s`)", memory.Title(s.Key), s.Key)
		if !s.UpdatedAt.IsZero() {
			fmt.Fprintf(&b, " · обновлено %s", s.UpdatedAt.In(l.env.Now().Location()).Format("2006-01-02 15:04"))
		}
		if s.UpdatedByMR > 0 {
			fmt.Fprintf(&b, " в !%d", s.UpdatedByMR)
		}
		if s.SourceNote != "" {
			fmt.Fprintf(&b, " · %s", s.SourceNote)
		}
		b.WriteString("</sub>\n\n")
		b.WriteString(memory.Render(memory.Bullets(s.Content)))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "_Всего %d из %d символов._ Очистить раздел: `%[3]s память очистить <раздел>` · всё: `%[3]s память очистить` · добавить: `%[3]s память запомни <раздел>: <текст>`\n",
		total, l.env.Cfg.Review.Memory.TotalCap(), l.env.Mention())
	return l.env.Reply(ctx, job, b.String())
}

// MemoryCommandsList extends "@bot help" (empty when memory is off).
func (l *Feature) MemoryCommandsList() string {
	if !l.memoryServiceOn() {
		return ""
	}
	return core.JobMemory.Help(l.env.Mention())
}
