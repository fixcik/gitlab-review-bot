package task

import (
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/jira"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/tasklink"
)

// SectionTitle is the heading of the task section of the summary note.
const SectionTitle = "### 📋 Соответствие задаче"

func verdictBadge(v string) string {
	switch v {
	case output.VerdictMatch:
		return "✅ соответствует"
	case output.VerdictPartial:
		return "🟡 частично"
	case output.VerdictMismatch:
		return "🔴 расходится"
	default:
		return "❔ неясно"
	}
}

// renderSection renders "📋 Соответствие задаче" for the summary note,
// or "" when Jira is off or the MR has no issue key.
func (t *Feature) renderSection(tc *Context, ta *output.TaskAlignment) string {
	if tc == nil || !tc.jiraOn || len(tc.keys) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(SectionTitle)
	b.WriteString("\n\n")
	byKey := map[string]*jira.Issue{}
	for _, is := range tc.Issues {
		byKey[is.Key] = is
	}
	failed := map[string]string{}
	for _, f := range tc.failures {
		failed[f.Key] = f.Reason
	}
	b.WriteString("**Задачи:**\n")
	for _, key := range tc.keys {
		is := byKey[key]
		fmt.Fprintf(&b, "- [%s](%s)", key, t.BrowseURL(key, is))
		switch {
		case is != nil:
			if s := mdtext.Inline(is.Summary, 150); s != "" {
				b.WriteString(" — " + s)
			}
			var meta []string
			if is.Type != "" {
				meta = append(meta, mdtext.Inline(is.Type, 40))
			}
			if is.Status != "" {
				meta = append(meta, mdtext.Inline(is.Status, 40))
			}
			if len(meta) > 0 {
				fmt.Fprintf(&b, " (%s)", strings.Join(meta, ", "))
			}
		case failed[key] != "":
			fmt.Fprintf(&b, " — ⚠️ не удалось получить из Jira: %s", failed[key])
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if len(tc.Issues) == 0 {
		b.WriteString("Проверка соответствия задаче не выполнена: ни одну задачу не удалось получить из Jira. Ревью кода выполнено как обычно.\n\n")
		return b.String()
	}
	if ta == nil {
		b.WriteString("**Вердикт:** " + verdictBadge(output.VerdictUnclear) + " — оценка соответствия не получена.\n\n")
		return b.String()
	}
	b.WriteString("**Вердикт:** " + verdictBadge(ta.Verdict) + "\n\n")
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		b.WriteString("**" + title + ":**\n")
		for _, it := range items {
			b.WriteString("- " + mdtext.OneLine(it) + "\n")
		}
		b.WriteString("\n")
	}
	list("Реализовано", ta.Implemented)
	list("Не хватает", ta.Missing)
	list("Сверх задачи", ta.Extra)
	if ta.Notes != "" {
		b.WriteString(ta.Notes)
		b.WriteString("\n\n")
	}
	return b.String()
}

// SummaryParts renders the title hint block and the task section for
// the summary note. prevSummary is the current summary body (its stored
// title suggestion is reused when Claude gave none).
func (t *Feature) SummaryParts(mr *core.MergeRequest, tc *Context, out *output.ReviewOutput, prevSummary string) (hint, section string) {
	if tc.titleCheck {
		sug := toSuggestion(out.SuggestedTitle)
		if sug == nil && tc.AskTitle {
			sug = ParseTitleSuggestion(prevSummary)
		}
		hint = renderTitleHint(tasklink.BuildHint(t.titleHintInput(mr, tc.found, sug, tc.projectKeys)), sug)
	}
	// Jira text and the model's: no "<!--" in the summary (summary.Block).
	return hint, mdtext.DefuseComments(t.renderSection(tc, out.TaskAlignment))
}
