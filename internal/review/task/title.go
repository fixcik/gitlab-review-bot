package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/tasklink"
)

// titleHintInput builds the tasklink input for mr.
func (t *Feature) titleHintInput(mr *core.MergeRequest, found []tasklink.Found, sug *tasklink.Suggestion, projectKeys []string) tasklink.HintInput {
	return tasklink.HintInput{
		Title:       mr.Title,
		Found:       found,
		Types:       t.TitleTypes(),
		ProjectKeys: projectKeys,
		RequireKey:  t.env.Cfg.Jira.RequireKeyInTitle,
		Suggestion:  sug,
	}
}

func toSuggestion(t *output.TitleSuggestion) *tasklink.Suggestion {
	if t == nil {
		return nil
	}
	return &tasklink.Suggestion{Type: t.Type, Scope: t.Scope, Description: t.Description}
}

// renderTitleHint renders the marker-delimited hint block. The block is
// always present (possibly empty) when the title check is on, so a later
// refresh can fill it in place.
func renderTitleHint(h tasklink.Hint, sug *tasklink.Suggestion) string {
	start := marker.TitleHintStart + "\n"
	if sug != nil {
		if js, err := json.Marshal(sug); err == nil { // HTML-escaped: no "<!--" or "-->" inside
			start += fmt.Sprintf("<!-- reviewbot:title-suggestion %s -->\n", js)
		}
	}
	var b strings.Builder // the MR title and the model's suggestion: defused by summary.Block
	if !h.Empty() {
		b.WriteString("> 💡 **Заголовок MR.** ")
		b.WriteString(strings.Join(h.Messages, " "))
		b.WriteString("\n>\n")
		fmt.Fprintf(&b, "> Предлагаемый заголовок: %s\n", mdtext.InlineCode(h.SuggestedTitle))
	}
	return summary.Block(start, b.String(), marker.TitleHintEnd)
}

// ParseTitleSuggestion extracts the stored suggestion from a summary body.
func ParseTitleSuggestion(body string) *tasklink.Suggestion {
	block := marker.TitleHintRe.FindString(body)
	m := marker.TitleSuggestionRe.FindStringSubmatch(block)
	if m == nil {
		return nil
	}
	var s tasklink.Suggestion
	if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
		return nil
	}
	return &s
}

// replaceTitleHint swaps the hint block in a summary body. Summaries
// without the block get it right after the title line (or the marker).
func replaceTitleHint(body, block string) string {
	if loc := marker.TitleHintRe.FindStringIndex(body); loc != nil {
		return body[:loc[0]] + block + body[loc[1]:]
	}
	lines := strings.Split(body, "\n")
	at := slices.IndexFunc(lines, summary.IsTitle)
	if at < 0 {
		at = slices.IndexFunc(lines, marker.IsSummaryNote)
	}
	if at < 0 {
		return block + "\n\n" + body
	}
	return strings.Join(summary.InsertAfter(lines, at, block), "\n")
}

// HandleRefreshTitle re-renders only the title hint of the existing summary
// note after the MR title changed. It never runs Claude, never checks or
// records limits and never creates a summary note.
func (t *Feature) HandleRefreshTitle(ctx context.Context, job core.Job) error {
	log := t.env.Log.With("mr", job.MRKey(), "kind", job.Kind)
	if !t.env.Cfg.Jira.TitleCheck {
		return nil
	}
	mr, err := t.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return fmt.Errorf("get mr: %w", err)
	}
	if mr.State != "opened" || mr.Draft {
		log.Info("review: refresh title: mr not open or draft, skipping")
		return nil
	}
	eff, ok, err := t.env.RepoConfig.Auto(ctx, job.ProjectID, mr)
	if !ok {
		return err
	}
	if !eff.Enabled {
		return nil
	}
	threads, err := t.pub.ScanMR(ctx, job)
	if err != nil {
		return err
	}
	if threads.SummaryID == 0 {
		log.Info("review: refresh title: no summary note yet")
		return nil
	}
	sug := ParseTitleSuggestion(threads.SummaryBody)
	hint := tasklink.BuildHint(t.titleHintInput(mr, t.findKeys(ctx, job, mr, eff.JiraProjectKeys), sug, eff.JiraProjectKeys))
	body := replaceTitleHint(threads.SummaryBody, renderTitleHint(hint, sug))
	body = mdtext.NeutralizeQuickActions(body)
	if body == threads.SummaryBody {
		return nil
	}
	if err := t.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, threads.SummaryID, body); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("update summary: %w", err)
	}
	log.Info("review: title hint refreshed", "hint", !hint.Empty())
	return nil
}
