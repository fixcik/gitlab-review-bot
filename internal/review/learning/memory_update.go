package learning

// Applying the captured memory_updates of a run to the project's memory and
// reporting what changed (summary block, chat reply).

import (
	"context"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

// maxMemoryUpdates caps how many updates of one answer are applied.
const maxMemoryUpdates = 20

// memoryResult is what one merge changed.
type memoryResult struct {
	changes []memory.Change
}

// applyCapturedMemory merges the memory_updates of the captured answer.
func (l *Feature) applyCapturedMemory(ctx context.Context, job core.Job, eff config.Effective, m *MemoryRun, source string) *memoryResult {
	if m == nil || !m.On {
		return nil
	}
	raw, turns, ok := m.captured()
	if !ok {
		return nil
	}
	var w struct {
		Updates []memory.Update `json:"memory_updates"`
	}
	if err := jsonschema.UnmarshalStrict(raw, &w); err != nil {
		l.env.Log.Warn("memory: parse updates", "mr", job.MRKey(), "err", err)
		return nil
	}
	// The turn count is informational: Claude decides what took more than
	// two steps; short runs may still send updates and long runs may send
	// none (no forced writes).
	l.env.Log.Info("memory: run finished", "mr", job.MRKey(), "turns", turns, "updates", len(w.Updates))
	if len(w.Updates) == 0 {
		return nil
	}
	if len(w.Updates) > maxMemoryUpdates {
		w.Updates = w.Updates[:maxMemoryUpdates]
	}
	changes, err := l.mergeMemory(ctx, job, w.Updates, source, true)
	if err != nil {
		l.env.Log.Warn("memory: merge", "mr", job.MRKey(), "err", err)
	}
	if len(changes) == 0 {
		return nil
	}
	return &memoryResult{changes: changes}
}

// mergeMemory applies updates to the project's memory and saves the changed
// sections. Over-cap sections are compacted by the light model when compact
// is set (and the global and learning limits allow), else the newest bullets
// are kept.
func (l *Feature) mergeMemory(ctx context.Context, job core.Job, updates []memory.Update, source string, compact bool) ([]memory.Change, error) {
	secs, err := l.memory.ListMemory(ctx, job.ProjectID)
	if err != nil {
		return nil, err
	}
	mem := map[string][]string{}
	for _, s := range secs {
		mem[s.Key] = memory.Bullets(s.Content)
	}
	before := map[string]string{}
	for _, k := range memory.Sections {
		before[k] = memory.Render(mem[k])
	}
	changes := memory.Apply(mem, updates)
	if len(changes) == 0 {
		return nil, nil
	}
	secCap, totalCap := l.env.Cfg.Review.Memory.SectionCap(), l.env.Cfg.Review.Memory.TotalCap()
	var over []string
	for _, k := range memory.Sections {
		mem[k] = memory.Dedupe(mem[k])
		if memory.Size(mem[k]) > secCap {
			over = append(over, k)
		}
	}
	if compact && len(over) > 0 {
		l.compactMemory(ctx, job, mem, over, secCap)
	}
	for _, k := range memory.Sections {
		mem[k] = memory.TrimNewest(mem[k], secCap)
	}
	memory.FitTotal(mem, totalCap)
	for _, k := range memory.Sections {
		content := memory.Render(mem[k])
		if content == before[k] {
			continue
		}
		if err := l.memory.SaveMemorySection(ctx, memory.Section{
			ProjectID: job.ProjectID, ProjectPath: job.ProjectPath, Key: k, Content: content,
			UpdatedAt: l.env.Now(), UpdatedByMR: job.MRIID, SourceNote: source,
		}); err != nil {
			return nil, err
		}
	}
	l.env.Log.Info("memory: updated", "mr", job.MRKey(), "project_id", job.ProjectID, "changes", len(changes))
	return changes, nil
}

// sectionTitles lists the titles of the changed sections in display order.
func (m *memoryResult) sectionTitles() string {
	have := map[string]bool{}
	for _, c := range m.changes {
		have[c.Section] = true
	}
	var t []string
	for _, k := range memory.Sections {
		if have[k] {
			t = append(t, memory.Title(k))
		}
	}
	return strings.Join(t, ", ")
}

func opName(op string) string {
	switch op {
	case memory.OpReplace:
		return "исправлено"
	case memory.OpRemove:
		return "удалено"
	}
	return "добавлено"
}

// memorySummary is the collapsed "📒 Память проекта" block for the summary
// note; "" when nothing changed.
func (l *Feature) memorySummary(m *memoryResult) string {
	if m == nil || len(m.changes) == 0 {
		return ""
	}
	n := len(m.changes)
	var b strings.Builder
	fmt.Fprintf(&b, "<details>\n<summary>📒 Память проекта: обновлено %d %s (%s)</summary>\n\n",
		n, mdtext.Plural(n, "пункт", "пункта", "пунктов"), m.sectionTitles())
	for _, c := range m.changes {
		fmt.Fprintf(&b, "- **%s** · %s: %s\n", memory.Title(c.Section), opName(c.Op), mdtext.Shorten(c.Text, 200))
	}
	fmt.Fprintf(&b, "\nПамять видна только боту (хранится в его базе, не в репозитории). Показать: `%[1]s память` · очистить раздел: `%[1]s память очистить <раздел>`\n\n</details>\n\n", l.env.Mention())
	return b.String()
}

// FinishReviewMemory applies the review's memory_updates and returns the
// summary block.
func (l *Feature) FinishReviewMemory(ctx context.Context, job core.Job, eff config.Effective, m *MemoryRun, head string) string {
	src := fmt.Sprintf("ревью !%d (%s)", job.MRIID, mdtext.ShortSHA(head))
	return l.memorySummary(l.applyCapturedMemory(ctx, job, eff, m, src))
}

// FinishChatMemory applies the chat answer's memory_updates and returns the
// text to append to the reply ("" when nothing changed).
func (l *Feature) FinishChatMemory(ctx context.Context, job core.Job, eff config.Effective, m *MemoryRun) string {
	res := l.applyCapturedMemory(ctx, job, eff, m, fmt.Sprintf("вопрос в !%d", job.MRIID))
	if res == nil { // nil unless something changed
		return ""
	}
	n := len(res.changes)
	return fmt.Sprintf("\n\n📒 Обновил память проекта: %d %s (%s). Показать: `%s память`.",
		n, mdtext.Plural(n, "пункт", "пункта", "пунктов"), res.sectionTitles(), l.env.Mention())
}
