package task_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/jira"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
)

func reviewOutWith(t *testing.T, ta *output.TaskAlignment, st *output.TitleSuggestion) string {
	return reviewtest.MustJSON(t, output.ReviewOutput{
		Summary:        "Добавлен LRU-кэш.",
		Walkthrough:    []output.FileSummary{{Path: "cache.go", Summary: "кэш"}},
		TaskAlignment:  ta,
		SuggestedTitle: st,
	})
}

func TestTaskAlignmentRendered(t *testing.T) {
	h := reviewtest.New(t)
	ft := &reviewtest.FakeTasks{Issues: map[string]*jira.Issue{"PROJ-1": reviewtest.CacheIssue()}}
	reviewtest.WithJira(h, ft)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutWith(t, &output.TaskAlignment{
		Verdict: "PARTIAL", Implemented: []string{"LRU-кэш"}, Missing: []string{"TTL"}, Extra: []string{"метрики"}, Notes: "TTL не настраивается.",
	}, &output.TitleSuggestion{Type: "feat", Description: "ignored: not asked"})}}

	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ft.Calls, ",") != "PROJ-1" {
		t.Fatalf("jira calls %v", ft.Calls)
	}
	req := h.Runner.Reqs[0]
	for _, s := range []string{"<untrusted_jira_issues>", "issue PROJ-1", "Нужен LRU-кэш с TTL.", "IGNORE PREVIOUS INSTRUCTIONS",
		"TTL = 5 минут", "Parent issue PROJ-0", "Epic PROJ-100", "task_alignment", "all of its changed files, compared with the code before it in /base"} {
		if !strings.Contains(req.Prompt, s) {
			t.Errorf("prompt lacks %q", s)
		}
	}
	if strings.Contains(req.Prompt, "MR title format") {
		t.Errorf("title suggestion must not be asked for a valid title")
	}
	props, required := reviewtest.SchemaProps(t, req.JSONSchema)
	if props["task_alignment"] == nil || props["suggested_title"] != nil || !strings.Contains(strings.Join(required, ","), "task_alignment") {
		t.Fatalf("schema props/required: %v %v", props, required)
	}

	sum := reviewtest.SummaryBody(t, h)
	for _, s := range []string{
		"### 📋 Соответствие задаче", "[PROJ-1](https://jira.example.com/browse/PROJ-1)", "LRU-кэш для API @\u200ball", "(Story, In Progress)",
		"**Вердикт:** 🟡 частично", "**Реализовано:**\n- LRU-кэш", "**Не хватает:**\n- TTL", "**Сверх задачи:**\n- метрики", "TTL не настраивается.",
	} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
	// Valid title with the key: empty hint block, no suggestion stored.
	if hb := reviewtest.HintBlock(sum); hb != marker.TitleHintStart+"\n"+marker.TitleHintEnd {
		t.Errorf("hint block: %q", hb)
	}
	if strings.Index(sum, "Добавлен LRU-кэш.") > strings.Index(sum, task.SectionTitle) {
		t.Errorf("task section must follow the summary text")
	}
}

func TestKeyOnlyInBranch(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Title = "feat(cache): добавить кэш"
	h.GL.MR.SourceBranch = "feature/proj-1-cache"
	ft := &reviewtest.FakeTasks{Issues: map[string]*jira.Issue{"PROJ-1": reviewtest.CacheIssue()}}
	reviewtest.WithJira(h, ft)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutWith(t, &output.TaskAlignment{Verdict: "match", Implemented: []string{"кэш"}}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ft.Calls, ",") != "PROJ-1" {
		t.Fatalf("jira calls %v", ft.Calls)
	}
	sum := reviewtest.SummaryBody(t, h)
	hb := reviewtest.HintBlock(sum)
	for _, s := range []string{"💡 **Заголовок MR.**", "**PROJ-1** найден в названии ветки", "`feat(cache): добавить кэш (PROJ-1)`"} {
		if !strings.Contains(hb, s) {
			t.Errorf("hint lacks %q:\n%s", s, hb)
		}
	}
	if !strings.Contains(sum, "**Вердикт:** ✅ соответствует") {
		t.Errorf("summary: %s", sum)
	}
}

func TestKeyFromCommitsAndDescriptionPriority(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Title = "fix: поправить кэш"
	h.GL.MR.Description = "Related: PROJ-7"
	h.GL.Commits = []core.Commit{{SHA: "b", Message: "fix: b PROJ-9"}, {SHA: "a", Message: "fix: a PROJ-8"}}
	ft := &reviewtest.FakeTasks{Issues: map[string]*jira.Issue{}}
	for _, k := range []string{"PROJ-7", "PROJ-8", "PROJ-9"} {
		ft.Issues[k] = &jira.Issue{Key: k, Summary: "s " + k}
	}
	reviewtest.WithJira(h, ft)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutWith(t, &output.TaskAlignment{Verdict: "unclear"}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	// Description first, then commits oldest first.
	if strings.Join(ft.Calls, ",") != "PROJ-7,PROJ-8,PROJ-9" {
		t.Fatalf("jira calls %v", ft.Calls)
	}
	if hb := reviewtest.HintBlock(reviewtest.SummaryBody(t, h)); !strings.Contains(hb, "в описании MR") || !strings.Contains(hb, "`fix: поправить кэш (PROJ-7)`") {
		t.Fatalf("hint: %s", hb)
	}
}

func TestInvalidTitleSuggestion(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Title = "Draft: Add cache PROJ-1"
	h.GL.MR.Draft = false // draft prefix in the title only
	ft := &reviewtest.FakeTasks{Issues: map[string]*jira.Issue{"PROJ-1": reviewtest.CacheIssue()}}
	reviewtest.WithJira(h, ft)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutWith(t, &output.TaskAlignment{Verdict: "match"},
		&output.TitleSuggestion{Type: "feat", Scope: "cache", Description: "добавить LRU-кэш"})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if !strings.Contains(req.Prompt, "MR title format") || !strings.Contains(req.Prompt, "feat, fix, refactor") {
		t.Fatalf("prompt must ask for a title")
	}
	props, required := reviewtest.SchemaProps(t, req.JSONSchema)
	st, _ := props["suggested_title"].(map[string]any)
	if st == nil || !strings.Contains(strings.Join(required, ","), "suggested_title") {
		t.Fatalf("schema: %v", props)
	}
	if enum := fmt.Sprint(st["properties"].(map[string]any)["type"].(map[string]any)["enum"]); !strings.Contains(enum, "revert") {
		t.Fatalf("type enum: %s", enum)
	}
	sum := reviewtest.SummaryBody(t, h)
	hb := reviewtest.HintBlock(sum)
	for _, s := range []string{"Conventional Commits", "`feat(cache): добавить LRU-кэш (PROJ-1)`", "reviewbot:title-suggestion"} {
		if !strings.Contains(hb, s) {
			t.Errorf("hint lacks %q:\n%s", s, hb)
		}
	}
	if sug := task.ParseTitleSuggestion(sum); sug == nil || sug.Description != "добавить LRU-кэш" {
		t.Fatalf("stored suggestion: %+v", sug)
	}
}

func TestJiraDisabledTitleCheckOnly(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Title = "feat: x"
	h.GL.MR.SourceBranch = "PROJ-5-x"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sum := reviewtest.SummaryBody(t, h)
	if strings.Contains(sum, task.SectionTitle) {
		t.Fatalf("no task section without Jira")
	}
	if hb := reviewtest.HintBlock(sum); !strings.Contains(hb, "`feat: x (PROJ-5)`") {
		t.Fatalf("hint: %s", hb)
	}

	h2 := reviewtest.New(t)
	h2.Cfg.Jira.TitleCheck = false
	h2.Cfg.Review.TBD.Enabled = false // the TBD block reads commits for the branch age
	h2.GL.MR.Title = "whatever"
	h2.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if sum := reviewtest.SummaryBody(t, h2); strings.Contains(sum, "reviewbot:title-hint") || h2.GL.CommitCalls != 0 {
		t.Fatalf("title check off: %s (commit calls %d)", sum, h2.GL.CommitCalls)
	}
}

func TestRefreshTitleReplacesOnlyHint(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Title = "Add cache"
	h.GL.MR.SourceBranch = "feature/PROJ-1"
	reviewtest.WithJira(h, &reviewtest.FakeTasks{Issues: map[string]*jira.Issue{"PROJ-1": reviewtest.CacheIssue()}})
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutWith(t, &output.TaskAlignment{Verdict: "match"},
		&output.TitleSuggestion{Type: "feat", Description: "добавить кэш"})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sid := h.GL.CreatedNotes[0]
	before := h.GL.Notes[sid]
	if !strings.Contains(reviewtest.HintBlock(before), "`feat: добавить кэш (PROJ-1)`") {
		t.Fatalf("initial hint: %s", reviewtest.HintBlock(before))
	}
	h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(sid, before, nil)}}}
	runs, recs := len(h.Runner.Reqs), len(h.Lim.Records())

	refresh := core.Job{Kind: core.JobRefreshTitle, ProjectID: reviewtest.PID, ProjectPath: "group/api", MRIID: reviewtest.IID}

	// Still invalid: Claude's stored suggestion is reused, key from title now.
	h.GL.MR.Title = "Add cache (PROJ-1)"
	if err := h.R().Handle(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	after := h.GL.Notes[sid]
	if !strings.Contains(reviewtest.HintBlock(after), "`feat: добавить кэш (PROJ-1)`") || strings.Contains(reviewtest.HintBlock(after), "названии ветки") {
		t.Fatalf("refreshed hint: %s", reviewtest.HintBlock(after))
	}

	// Fixed title: hint disappears, the rest of the note is byte-identical.
	h.GL.Discussions[0].Notes[0].Body = after
	h.GL.MR.Title = "feat: добавить кэш (PROJ-1)"
	if err := h.R().Handle(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	fixed := h.GL.Notes[sid]
	hb := reviewtest.HintBlock(fixed)
	if strings.Contains(hb, "Заголовок MR") {
		t.Fatalf("hint must be gone: %s", hb)
	}
	if strings.Replace(fixed, hb, "", 1) != strings.Replace(before, reviewtest.HintBlock(before), "", 1) {
		t.Fatalf("only the hint block may change:\nbefore:\n%s\nafter:\n%s", before, fixed)
	}
	if len(h.Runner.Reqs) != runs || len(h.Lim.Records()) != recs || len(h.Lim.UserCalls) != 0 {
		t.Fatalf("refresh must not run Claude or touch limits: runs %d records %d user checks %d", len(h.Runner.Reqs), len(h.Lim.Records()), len(h.Lim.UserCalls))
	}
	if len(h.GL.Replies) != 0 || len(h.GL.CreatedNotes) != 1 || h.Repos.Checkouts != 1 {
		t.Fatalf("refresh side effects: replies %v created %v checkouts %d", h.GL.Replies, h.GL.CreatedNotes, h.Repos.Checkouts)
	}
	if len(h.Deps.Tasks.(*reviewtest.FakeTasks).Calls) != 1 {
		t.Fatalf("refresh must not call Jira")
	}
}

func TestRefreshTitleWithoutSummaryOrStoredSuggestion(t *testing.T) {
	h := reviewtest.New(t)
	refresh := core.Job{Kind: core.JobRefreshTitle, ProjectID: reviewtest.PID, ProjectPath: "group/api", MRIID: reviewtest.IID}
	h.GL.MR.Title = "Add cache"
	if err := h.R().Handle(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.CreatedNotes)+len(h.GL.UpdatedNotes)+len(h.GL.Replies) != 0 {
		t.Fatalf("no summary: nothing must be written")
	}

	// A summary written before this feature (no hint block): inserted after
	// the title; no stored suggestion -> template from the title.
	old := marker.Summary("abc123") + "\n" + summary.Title + "\n\nСтарое резюме.\n"
	h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(500, old, nil)}}}
	h.GL.MR.Title = "feature: cache"
	h.GL.MR.SourceBranch = "PROJ-3-cache"
	if err := h.R().Handle(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	body := h.GL.Notes[500]
	if !strings.Contains(body, summary.Title+"\n\n"+marker.TitleHintStart) || !strings.Contains(body, "`feat: cache (PROJ-3)`") || !strings.Contains(body, "Старое резюме.") {
		t.Fatalf("body:\n%s", body)
	}
	if len(h.Runner.Reqs) != 0 || len(h.Lim.Records()) != 0 {
		t.Fatal("no claude")
	}

	// Drafts are skipped.
	h.GL.MR.Draft = true
	h.GL.UpdatedNotes = nil
	if err := h.R().Handle(context.Background(), refresh); err != nil || len(h.GL.UpdatedNotes) != 0 {
		t.Fatalf("draft: %v %v", err, h.GL.UpdatedNotes)
	}
}

func TestRefreshKindHandledExplicitly(t *testing.T) {
	// JobRefreshTitle reports Manual() == true; the reviewer must route it
	// to the refresh handler and never to the review flow (which would do a
	// permission check for user 0 and charge limits).
	if !core.JobRefreshTitle.Manual() {
		t.Skip("Manual() semantics changed")
	}
	h := reviewtest.New(t)
	h.GL.Access = 0
	if err := h.R().Handle(context.Background(), core.Job{Kind: core.JobRefreshTitle, ProjectID: reviewtest.PID, MRIID: reviewtest.IID}); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Replies) != 0 || len(h.Lim.UserCalls) != 0 {
		t.Fatalf("refresh went through the command path")
	}
}

func TestRepoJiraProjectKeysOverride(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Title = "feat: оплата (PAY-7)"
	h.GL.RepoConfig = "jira:\n  project_keys: [pay]\n"
	ft := &reviewtest.FakeTasks{Issues: map[string]*jira.Issue{"PAY-7": {Key: "PAY-7", Summary: "Оплата", URL: "https://jira.example.com/browse/PAY-7"}}}
	reviewtest.WithJira(h, ft) // service keys: PROJ only
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutWith(t, &output.TaskAlignment{Verdict: "match"}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ft.Calls, ",") != "PAY-7" {
		t.Fatalf("repo keys not used, jira calls %v", ft.Calls)
	}
	if hb := reviewtest.HintBlock(reviewtest.SummaryBody(t, h)); strings.Contains(hb, "💡") {
		t.Errorf("valid title with repo key must not get a hint:\n%s", hb)
	}
}

func TestServiceJiraProjectKeysWithoutRepoOverride(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Title = "feat: оплата (PAY-7)"
	ft := &reviewtest.FakeTasks{}
	reviewtest.WithJira(h, ft) // service keys: PROJ only, PAY is not a task key here
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutWith(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(ft.Calls) != 0 {
		t.Fatalf("PAY-7 must be ignored with service keys [PROJ], calls %v", ft.Calls)
	}
	if hb := reviewtest.HintBlock(reviewtest.SummaryBody(t, h)); !strings.Contains(hb, "PROJ-?") {
		t.Errorf("hint should suggest a PROJ key:\n%s", hb)
	}
}

// The suggested title is asked in English by default, whatever the review
// language is; a service title prompt applies, and the repository's
// overrides it.
func TestTitlePromptLanguageAndOverride(t *testing.T) {
	run := func(t *testing.T, h *reviewtest.Harness) string {
		t.Helper()
		h.GL.MR.Title = "Add cache PROJ-1"
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewOutWith(t, nil, &output.TitleSuggestion{Type: "feat", Description: "add cache"})}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		p := h.Runner.Reqs[0].Prompt
		i := strings.Index(p, "## MR title format")
		if i < 0 {
			t.Fatalf("no title section:\n%s", p)
		}
		sec := p[i:]
		if j := strings.Index(sec[3:], "\n## "); j >= 0 {
			sec = sec[:j+3]
		}
		return sec
	}
	h := reviewtest.New(t)
	h.Cfg.Review.DefaultLang = "ru"
	sec := run(t, h)
	if !strings.Contains(sec, "in English") || strings.Contains(sec, "in ru") || strings.Contains(sec, "Russian") ||
		!strings.Contains(sec, "72 characters") || !strings.Contains(sec, `Fill "suggested_title"`) {
		t.Errorf("default title section:\n%s", sec)
	}

	h = reviewtest.New(t)
	h.Cfg.Jira.TitlePrompt, h.Cfg.Jira.TitleLanguage = "Team format: {types} / {language}.", "de"
	if sec := run(t, h); !strings.Contains(sec, "Team format: feat, fix, refactor") || !strings.Contains(sec, "/ German.") || strings.Contains(sec, "72 characters") {
		t.Errorf("service title prompt:\n%s", sec)
	}

	h = reviewtest.New(t)
	h.Cfg.Jira.TitlePrompt = "Service format."
	h.GL.RepoConfig = "jira:\n  title_prompt: \"Repo format in {language}.\"\n  title_language: ru\n"
	if sec := run(t, h); !strings.Contains(sec, "Repo format in Russian.") || strings.Contains(sec, "Service format.") {
		t.Errorf("repo title prompt:\n%s", sec)
	}
}
