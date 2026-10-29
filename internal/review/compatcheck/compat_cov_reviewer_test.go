package compatcheck_test

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func compatProblems(ph *reviewtest.ProbHarness) string {
	got := ph.St.Kinds()[problems.CompatFailed]
	sort.Strings(got)
	return strings.Join(got, "|")
}

// With the compat check off the API changelog is still built: a failed
// run gives no changelog and a compat_failed problem; specs it could not
// compare are named in the summary (skipped ones are not) and recorded as
// problems; the review itself goes on and nothing blocks.
func TestAPISummaryCompatOffFailures(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	ph.Cfg.Review.CompatCheck = false
	ph.Compat.Err = errors.New("git exploded")
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	if len(ph.Compat.Calls) != 1 || !ph.Compat.Calls[0].SummaryOnly {
		t.Fatalf("calls %+v", ph.Compat.Calls)
	}
	sum := ph.Summary(t)
	if strings.Contains(sum, "📘") || strings.Contains(sum, "Сводка изменений API") || !strings.Contains(sum, "✅ Блокирующих замечаний нет.") {
		t.Errorf("summary:\n%s", sum)
	}
	if got := compatProblems(ph); got != "проверка совместимости: внутренняя ошибка" {
		t.Errorf("problems %q", got)
	}

	ph = reviewtest.NewProbHarness(t, "")
	ph.Cfg.Review.CompatCheck = false
	ph.Compat.Res = &compat.Result{Issues: []compat.Issue{
		{Source: compat.SourceOasdiff, Path: "api/swagger.yaml", Kind: compat.IssueUnavailable, Reason: "oas2to3 не установлен"},
		{Source: compat.SourceOasdiff, Path: "api/b.yaml", Kind: compat.IssueFailed, Reason: "exit status 2"},
		{Source: compat.SourceOasdiff, Path: "api/c.yaml", Kind: compat.IssueSkipped, Reason: "слишком большой"},
	}}
	if err := ph.Review(t); err != nil {
		t.Fatal(err)
	}
	sum = ph.Summary(t)
	for _, s := range []string{
		"ℹ️ Сводка изменений API для `api/swagger.yaml` не построена: oas2to3 не установлен.",
		"ℹ️ Сводка изменений API для `api/b.yaml` не построена: exit status 2.",
	} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
	if strings.Contains(sum, "api/c.yaml") || strings.Contains(sum, compatcheck.Badge) {
		t.Errorf("skipped spec or a compat section with the check off:\n%s", sum)
	}
	if got := compatProblems(ph); got != "oasdiff api/b.yaml: упала|oasdiff api/swagger.yaml: недоступна" {
		t.Errorf("problems %q", got)
	}
}

// The compat section and the prompt with deleted specs, unconfirmed
// warnings (listed, not blocking) and a check that failed.
func TestCompatSectionDeletedWarningsFailed(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	ph.Compat.Res = &compat.Result{
		Deleted:  []compat.Spec{{Kind: compat.KindOpenAPI, Path: "api/old.yaml"}},
		Warnings: []compat.Change{{Source: compat.SourceOasdiff, Path: "api/a.yaml", Message: "enum value `x` removed?"}},
		Issues: []compat.Issue{
			{Source: compat.SourceBuf, Path: "proto/a.proto", Kind: compat.IssueFailed, Reason: "buf: exit 100"},
			{Source: compat.SourceGraphQL, Kind: compat.IssueSkipped, Reason: "нет схемы"},
		},
	}
	if err := ph.Review(t, reviewtest.RunResp{Out: reviewtest.ReviewOutCompat(t, nil, nil)}); err != nil {
		t.Fatal(err)
	}
	p := ph.Runner.Reqs[0].Prompt
	for _, s := range []string{
		"Spec files deleted by this merge request (everything they described is gone for clients of the previous version):\n- api/old.yaml",
		"<untrusted_tool_warnings>\n- [oasdiff] api/a.yaml: enum value `x` removed?",
		"These checks could not be done: buf (proto/a.proto).",
	} {
		if !strings.Contains(p, s) {
			t.Errorf("prompt lacks %q:\n%s", s, p)
		}
	}
	sum := ph.Summary(t)
	for _, s := range []string{
		"✅ Несовместимых изменений не найдено.",
		"⚠️ Проверка buf не выполнена (`proto/a.proto`): buf: exit 100.",
		"ℹ️ " + compat.SourceGraphQL + ": нет схемы.",
		"<summary>Возможные несовместимости, которые инструменты не смогли подтвердить (1, не блокируют)</summary>",
		"- **oasdiff** · `api/a.yaml` — enum value",
		"✅ Блокирующих замечаний нет.",
	} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
	if got := compatProblems(ph); got != "buf proto/a.proto: упала" {
		t.Errorf("problems %q", got)
	}
}

// A breaking-change thread that could not be created is still a breaking
// change: listed (blocking) and the summary asks to check it by hand.
func TestCompatThreadNotCreated(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.FailDraftFor = "removed the optional property 'email'"
	h.Compat.Res = &compat.Result{Changes: []compat.Change{reviewtest.OasdiffChange}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(t.Context(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(reviewtest.CompatThreads(h)); n != 0 {
		t.Fatalf("threads %d", n)
	}
	sum := reviewtest.SummaryBody(t, h)
	for _, s := range []string{
		"⛔ **Найдено 1 несовместимое изменение** — мерж заблокирован до исправления",
		"⚠️ Не удалось создать 1 тред для несовместимых изменений — проверьте список выше вручную.",
	} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
}

// A fixed deterministic thread GitLab refuses to resolve stays open: it is
// not counted as fixed and is listed as still blocking.
func TestCompatFixedResolveFails(t *testing.T) {
	h := reviewtest.New(t)
	tool := compatcheck.DeterministicItem(reviewtest.OasdiffChange)
	pos := &core.Position{NewPath: "cache.go", OldPath: "cache.go", NewLine: 3}
	h.GL.Discussions = []core.Discussion{{ID: "d-tool", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1, compatcheck.NoteBody(tool, false, 0), pos)}}}
	h.GL.FailResolveFor = "d-tool"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(t.Context(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	r := reviewtest.LastReply(t, h.GL)
	if r.DiscussionID != "d-tool" || !strings.Contains(r.Body, "Не удалось снять тред автоматически") || !strings.Contains(r.Body, marker.Fixed) {
		t.Errorf("reply %+v", r)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, "**oasdiff** · `cache.go:3` — ") || !strings.Contains(sum, "_(найдено ранее, тред открыт)_") ||
		strings.Contains(sum, "Исправлено с прошлого ревью") || !strings.Contains(sum, "⛔ **Мерж заблокирован:**") {
		t.Errorf("summary:\n%s", sum)
	}
}

// Items whose thread is open or was closed by hand are skipped; the items
// after them are still published.
func TestCompatPublishSkipsOpenAndDismissed(t *testing.T) {
	h := reviewtest.New(t)
	open := compatcheck.Item{ID: "open1", Source: compat.SourceOasdiff, Path: "cache.go", Line: 2, Title: "open one"}
	dismissed := compatcheck.Item{ID: "dism1", Source: compat.SourceOasdiff, Path: "cache.go", Line: 3, Title: "dismissed one"}
	fresh := compatcheck.Item{ID: "new1", Source: compat.SourceOasdiff, Path: "cache.go", Line: 4, Title: "new one"}
	cr := &compatcheck.Run{Enabled: true, Items: []compatcheck.Item{open, dismissed, fresh}}
	threads := &mrthread.Threads{
		OpenCompat:      map[string]mrthread.OpenCompat{"open1": {DiscussionID: "d1"}},
		DismissedCompat: map[string]mrthread.OpenCompat{"dism1": {DiscussionID: "d2"}},
	}
	h.Internals().Compat.Publish(t.Context(), reviewtest.AutoJob(), &h.GL.MR, h.Idx, cr, threads)
	if len(cr.Published) != 1 || cr.Published[0].ID != "new1" || len(cr.Outside) != 0 {
		t.Fatalf("published %+v outside %+v", cr.Published, cr.Outside)
	}
	if d := h.GL.Published; len(d) != 1 || !strings.Contains(d[0].Body, "new one") || d[0].Pos == nil || d[0].Pos.NewLine != 4 {
		t.Fatalf("published %+v", d)
	}
	if sec := compatcheck.RenderSection(cr); !strings.Contains(sec, "Тред закрыт вручную") || !strings.Contains(sec, "dismissed one") {
		t.Errorf("section:\n%s", sec)
	}
}

// Without Claude's compatibility section an open Claude thread is not
// fixed: it stays open and blocking.
func TestCompatResolveFixedWithoutClaudeSection(t *testing.T) {
	h := reviewtest.New(t)
	cl := compatcheck.ClaudeItem(dropColumn)
	threads := &mrthread.Threads{OpenCompat: map[string]mrthread.OpenCompat{cl.ID: {DiscussionID: "d-c", Source: compatcheck.SourceClaude, Title: cl.Title}},
		CompatOrder: []string{cl.ID}}
	cr := &compatcheck.Run{Enabled: true, Res: &compat.Result{}}
	if n := h.Internals().Compat.ResolveFixed(t.Context(), reviewtest.AutoJob(), cr, &output.ReviewOutput{}, threads); n != 0 {
		t.Errorf("pending %d", n)
	}
	if len(cr.Remaining) != 1 || cr.Closed != 0 || len(h.GL.Replies) != 0 {
		t.Errorf("remaining %+v closed %d replies %+v", cr.Remaining, cr.Closed, h.GL.Replies)
	}
}
