package linters_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/jira"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// todoFile sets cache.go of the harness: line 2 (added) is line2, line 3
// (added) is "c", the rest unchanged.
func todoFile(h *reviewtest.Harness, line2 string, above ...string) {
	pre := strings.Join(append([]string{"a"}, above...), "\n")
	h.Repos.Diff = fmt.Sprintf("diff --git a/cache.go b/cache.go\n--- a/cache.go\n+++ b/cache.go\n@@ -1,3 +1,%d @@\n", 5+len(above))
	for _, l := range strings.Split(pre, "\n") {
		if l == "a" {
			h.Repos.Diff += " a\n"
		} else {
			h.Repos.Diff += "+" + l + "\n"
		}
	}
	h.Repos.Diff += "+" + line2 + "\n+c\n d\n e\n"
	h.Repos.Show = map[string]string{"cache.go": pre + "\n" + line2 + "\nc\nd\ne\n"}
}

// todoHarness: no external linters (empty fake checker), only the todo
// check of the bot itself.
func todoHarness(t *testing.T, line2 string) *reviewtest.Harness {
	h, _ := reviewtest.LintHarness(t)
	todoFile(h, line2)
	return h
}

func todoThreads(h *reviewtest.Harness) []reviewtest.DraftNote {
	var out []reviewtest.DraftNote
	for _, d := range reviewtest.LintThreads(h) {
		if _, tool, _ := marker.ParseLint(d.Body); tool == lint.ToolTodo {
			out = append(out, d)
		}
	}
	return out
}

func lastSummary(h *reviewtest.Harness) string {
	return h.GL.Notes[h.GL.CreatedNotes[len(h.GL.CreatedNotes)-1]]
}

func TestTodoWithoutTaskBecomesThread(t *testing.T) {
	h := todoHarness(t, "// TODO: rework cache")
	h.Cfg.Jira.ProjectKeys = []string{"PROJ", "OPS"}
	m := reviewtest.WithModerator(h)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	th := todoThreads(h)
	if len(th) != 1 {
		t.Fatalf("published: %+v", h.GL.Published)
	}
	body := th[0].Body
	for _, s := range []string{"**🔧 todo** · TODO без задачи · 🟡 Minor", "_Не блокирует мерж._", "> TODO: rework cache",
		"```text\nTODO(PROJ-123): …\n```", "Формат ключа: `PROJ-123` (проекты Jira: `PROJ`, `OPS`)", "`group/project#123`",
		"_Результат проверки todo (автоматическая проверка, без участия Claude)._"} {
		if !strings.Contains(body, s) {
			t.Errorf("thread lacks %q:\n%s", s, body)
		}
	}
	if strings.Contains(body, marker.Blocking) || th[0].Pos == nil || th[0].Pos.NewLine != 2 {
		t.Errorf("non-blocking inline thread expected: %+v", th[0].Pos)
	}
	if len(m.Resolved) != 1 {
		t.Errorf("non-blocking thread must be resolved by the service token: %v", m.Resolved)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, "| todo | 1 на изменённых строках |") || !strings.Contains(sum, "🔧 Линтеры и сканеры: 1 замечание") {
		t.Errorf("summary:\n%s", sum)
	}
	if p := h.Runner.Reqs[0].Prompt; !strings.Contains(p, "[todo TODO] cache.go:2 (minor): TODO без задачи") {
		t.Errorf("prompt:\n%s", p)
	}
}

func TestTodoWithTaskNothing(t *testing.T) {
	for _, line := range []string{"// TODO(PROJ-1): rework cache", "// FIXME see #12", `var s = "TODO list"`} {
		h := todoHarness(t, line)
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		if n := len(todoThreads(h)); n != 0 {
			t.Errorf("%q: %d threads", line, n)
		}
		if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "| todo | ✅ замечаний нет |") {
			t.Errorf("%q: summary:\n%s", line, sum)
		}
	}
}

func TestTodoThreadClosedWhenKeyAdded(t *testing.T) {
	h := todoHarness(t, "// TODO: rework cache")
	h.GL.PublishMakesThreads = true
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(todoThreads(h)); n != 1 {
		t.Fatalf("first review: %d", n)
	}
	// The comment moved down by two lines: the same thread, no repost.
	h.GL.MR.DiffRefs.HeadSHA = "head2"
	todoFile(h, "// TODO: rework cache", "x", "y")
	before := len(h.GL.Published)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobFullReview)); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Published) != before || strings.Contains(fmt.Sprint(h.GL.Replies), marker.Fixed) {
		t.Fatalf("moved TODO reposted or closed: %+v %+v", h.GL.Published[before:], h.GL.Replies)
	}
	if sum := lastSummary(h); !strings.Contains(sum, "уже обсуждаются: 1") {
		t.Errorf("summary:\n%s", sum)
	}
	// The key is added in the next push: the thread is closed as fixed.
	h.GL.MR.DiffRefs.HeadSHA = "head3"
	todoFile(h, "// TODO(PROJ-7): rework cache", "x", "y")
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobFullReview)); err != nil {
		t.Fatal(err)
	}
	var fixed []reviewtest.Reply
	for _, r := range h.GL.Replies {
		if strings.Contains(r.Body, marker.Fixed) {
			fixed = append(fixed, r)
		}
	}
	if len(fixed) != 1 || !strings.HasPrefix(fixed[0].DiscussionID, "pub") || !strings.Contains(fixed[0].Body, "проверка todo больше не находит") {
		t.Fatalf("replies: %+v", h.GL.Replies)
	}
	if sum := lastSummary(h); !strings.Contains(sum, "закрыто как исправленные: 1") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestTodoRepoDisabled(t *testing.T) {
	h := todoHarness(t, "// TODO: rework cache")
	h.GL.RepoConfig = "todo:\n  enabled: false\n"
	f := lint.Finding{Tool: lint.ToolTodo, Rule: "TODO", Path: "cache.go", Line: 2, Severity: core.SeverityMinor,
		Message: "TODO без задачи — «TODO: old»", Todo: &lint.TodoInfo{Keyword: "TODO", Comment: "TODO: old"}}
	h.GL.Discussions = []core.Discussion{{ID: "d-todo", Resolvable: true,
		Notes: []core.Note{reviewtest.BotNote(1, linters.NoteBody(f.ID(), f, false, false, false), &core.Position{NewPath: "cache.go", NewLine: 2})}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(todoThreads(h)); n != 0 || len(h.GL.Replies) != 0 {
		t.Fatalf("todo off: %d threads, replies %+v (old threads stay as they are)", n, h.GL.Replies)
	}
	if sum := reviewtest.SummaryBody(t, h); strings.Contains(sum, "| todo |") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestTodoRunsWithLintOff(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	todoFile(h, "# FIXME: later")
	h.Cfg.Review.Lint.Enabled = false
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	// cache.go is Go: "#" is not a comment there.
	if len(fl.Calls) != 0 || len(todoThreads(h)) != 0 {
		t.Fatalf("calls %d threads %d", len(fl.Calls), len(todoThreads(h)))
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "| todo | ✅ замечаний нет |") || strings.Contains(sum, "gitleaks") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestTodoBlocking(t *testing.T) {
	h := todoHarness(t, "// HACK: skip the check")
	h.GL.RepoConfig = "todo:\n  blocking: true\n  severity: critical\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	th := todoThreads(h)
	if len(th) != 1 || !strings.Contains(th[0].Body, marker.Blocking) || !strings.Contains(th[0].Body, "⛔ **Блокирует мерж.**") ||
		!strings.Contains(th[0].Body, "HACK без задачи · 🔴 Critical") {
		t.Fatalf("threads %+v", th)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "⛔ **Мерж заблокирован:** 1 блокирующее") {
		t.Errorf("summary:\n%s", sum)
	}
	// blocking: true with the default minor severity stays below
	// block_min_severity (critical): not blocking.
	h = todoHarness(t, "// HACK: skip the check")
	h.GL.RepoConfig = "todo:\n  blocking: true\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if th := todoThreads(h); len(th) != 1 || strings.Contains(th[0].Body, marker.Blocking) {
		t.Fatalf("minor must not block: %+v", th)
	}
}

func TestTodoClaudeDuplicateDropped(t *testing.T) {
	h := todoHarness(t, "// TODO: rework cache")
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 2, Severity: "minor", Title: "TODO без ссылки на задачу", Body: "заведите задачу"},
		{Path: "cache.go", Line: 3, Severity: "major", Title: "Гонка при записи в кэш", Body: "нужен мьютекс"},
	}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	var claude []string
	for _, d := range h.GL.Published {
		if _, ok := marker.ParseFinding(d.Body); ok {
			claude = append(claude, d.Body)
		}
	}
	if len(claude) != 1 || !strings.Contains(claude[0], "Гонка") || len(todoThreads(h)) != 1 {
		t.Fatalf("claude %v, todo %d", claude, len(todoThreads(h)))
	}
}

func TestTodoVerifyKeys(t *testing.T) {
	ft := &reviewtest.FakeTasks{Issues: map[string]*jira.Issue{"PROJ-1": {Key: "PROJ-1"}}}
	h := todoHarness(t, "// TODO(PROJ-999): rework cache")
	reviewtest.WithJira(h, ft)
	h.Cfg.Review.Todo.VerifyKeys = true
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	th := todoThreads(h)
	if len(th) != 1 || !strings.Contains(th[0].Body, "**🔧 todo** · задача PROJ-999 не найдена · 🟡 Minor") ||
		!strings.Contains(th[0].Body, "в Jira её нет или у бота нет к ней доступа") {
		t.Fatalf("threads %+v", th)
	}
	if strings.Count(strings.Join(ft.Calls, ","), "PROJ-999") != 1 {
		t.Errorf("calls %v", ft.Calls)
	}
	// The next review reuses the cached answer.
	h.GL.MR.DiffRefs.HeadSHA = "head2"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobFullReview)); err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.Join(ft.Calls, ","), "PROJ-999") != 1 {
		t.Errorf("cached key looked up again: %v", ft.Calls)
	}

	// An existing key: nothing.
	h = todoHarness(t, "// TODO(PROJ-1): rework cache")
	reviewtest.WithJira(h, ft)
	h.Cfg.Review.Todo.VerifyKeys = true
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(todoThreads(h)); n != 0 {
		t.Fatalf("existing key: %d threads", n)
	}

	// verify_keys off (default): keys are not looked up.
	ft2 := &reviewtest.FakeTasks{}
	h = todoHarness(t, "// TODO(PROJ-999): rework cache")
	reviewtest.WithJira(h, ft2)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(todoThreads(h)) != 0 || strings.Contains(strings.Join(ft2.Calls, ","), "PROJ-999") {
		t.Fatalf("verify_keys off: calls %v", ft2.Calls)
	}
}

func TestTodoVerifyKeysCap(t *testing.T) {
	ft := &reviewtest.FakeTasks{}
	h, _ := reviewtest.LintHarness(t)
	var lines []string
	for i := 1; i <= 12; i++ {
		lines = append(lines, fmt.Sprintf("// TODO(PROJ-%d): item", 100+i))
	}
	h.Repos.Diff = "diff --git a/todo.go b/todo.go\nnew file mode 100644\n--- /dev/null\n+++ b/todo.go\n@@ -0,0 +1,12 @@\n+" + strings.Join(lines, "\n+") + "\n"
	reviewtest.WithJira(h, ft)
	h.Cfg.Review.Todo.VerifyKeys = true
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range ft.Calls {
		if strings.HasPrefix(c, "PROJ-1") && len(c) == len("PROJ-101") {
			n++
		}
	}
	if n != 10 {
		t.Fatalf("lookups %d: %v", n, ft.Calls)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, "| todo | 10 на изменённых строках |") || !strings.Contains(sum, "не больше 10 запросов за ревью, не проверено: 2") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestTodoVerifyKeysJiraError(t *testing.T) {
	ph := reviewtest.NewProbHarness(t, "")
	ph.Deps.Lint = &reviewtest.FakeLint{}
	todoFile(ph.Harness, "// TODO(PROJ-5): rework cache")
	reviewtest.WithJira(ph.Harness, &reviewtest.FakeTasks{Errs: map[string]error{"PROJ-5": errors.New("jira: 502")}})
	ph.Cfg.Review.Todo.VerifyKeys = true
	if err := ph.Review(t, reviewtest.RunResp{Out: reviewtest.ReviewOutCompat(t, nil, nil)}); err != nil {
		t.Fatal(err)
	}
	if n := len(todoThreads(ph.Harness)); n != 0 {
		t.Fatalf("a Jira error is not a finding: %d", n)
	}
	k := ph.St.Kinds()
	if !strings.Contains(strings.Join(k[problems.JiraFailed], "|"), "todo: проверка ключей задач в комментариях") {
		t.Errorf("problems %v", k)
	}
	if sum := ph.Summary(t); !strings.Contains(sum, "Проверка todo выполнена частично: не удалось проверить в Jira ключей задач: 1") {
		t.Errorf("summary:\n%s", sum)
	}
}
