package linters

// The "todo" check (internal/lint/todo.go, review.todo): TODO / FIXME / XXX
// / HACK comments added by the MR must reference a task. It runs in the bot
// (no sandbox, no Claude) right after the linters and joins the lint step
// as the tool "todo": same threads (`<!-- reviewbot:lint … tool=todo -->`,
// id = file + rule + normalized comment text, so moving the line does not
// repost it), same auto-close when the comment gets a key or disappears,
// same row in the "🔧 Линтеры и сканеры" summary block, same dedupe of
// Claude's findings. It works when review.lint is off too.
//
// verify_keys looks referenced Jira keys up through task.Feature.Jira
// (review.Deps.Tasks; read-only, at most config.MaxTodoKeyLookups lookups
// per review, cached for todoKeyTTL); Jira errors never fail the review
// (problem jira_failed, the tool is "partial" so its threads are not
// auto-closed on a guess).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// RunTodoOnly is Run when review.lint is off for the repository: the
// todo check still runs on its own (nil when it is off too).
func (lf *Feature) RunTodoOnly(ctx context.Context, job core.Job, co core.Checkout, mr *core.MergeRequest, eff config.Effective, from, diff string) *Run {
	if !eff.Todo.Enabled {
		return nil
	}
	lr := &Run{Cfg: eff.Lint, Threads: map[string]Thread{}}
	lf.scanLintThreads(ctx, job, lr)
	lr.Res = &lint.Result{}
	lf.runTodo(ctx, job, co, mr, eff, from, diff, lr)
	return lr
}

// runTodo adds the todo results and status to lr (never fails the review).
func (lf *Feature) runTodo(ctx context.Context, job core.Job, co core.Checkout, mr *core.MergeRequest, eff config.Effective, from, diff string, lr *Run) {
	ctx, tsp := tracing.Start(ctx, "review.todo")
	defer tsp.End()
	if lr == nil || lr.Res == nil || !eff.Todo.Enabled {
		return
	}
	base := diffcap.FullDiffBase(mr)
	if from != base {
		d, err := co.Diff(ctx, base, eff.Ignore)
		if err != nil {
			lf.env.Log.Warn("review: todo: diff", "mr", job.MRKey(), "err", err)
			lr.Res.Tools = append(lr.Res.Tools, lint.ToolStatus{Tool: lint.ToolTodo, State: lint.StateFailed, Reason: "не удалось получить дифф MR"})
			return
		}
		diff = d
	}
	t := eff.Todo
	cfg := lint.TodoConfig{Keywords: t.Keywords, RequireTask: t.RequireTask, Severity: t.Severity, Blocking: t.Blocking,
		Ignore: t.Ignore, ProjectKeys: eff.JiraProjectKeys}
	var kc *todoKeyRun
	if t.VerifyKeys && lf.task.Jira != nil {
		kc = &todoKeyRun{lf: lf}
		cfg.CheckKey = kc.check
	}
	fs, st := lint.CheckTodo(ctx, lint.TodoRequest{Checkout: co, Head: mr.DiffRefs.HeadSHA, Diff: diff, Config: cfg})
	lr.Res.Findings = append(lr.Res.Findings, fs...)
	lr.Res.Tools = append(lr.Res.Tools, st)
	// The tool is on for this review: its fixed threads may be closed.
	lr.Cfg.Tools = append(append([]string{}, lr.Cfg.Tools...), lint.ToolTodo)
	if kc != nil && kc.skipped > 0 {
		lr.Res.Notes = append(lr.Res.Notes, fmt.Sprintf("todo: ключи задач проверены в Jira не все — не больше %d запросов за ревью, не проверено: %d.",
			config.MaxTodoKeyLookups, kc.skipped))
	}
	if st.State != lint.StateRan && st.State != lint.StateNotApplicable {
		lf.env.Log.Info("lint: tool issue", "tool", lint.ToolTodo, "state", st.State, "reason", st.Reason)
	}
}

const (
	// todoKeyTTL: how long a lookup result is reused.
	todoKeyTTL = time.Hour
	// todoKeyCacheMax bounds the cache (it is cleared when full).
	todoKeyCacheMax = 5000
)

// todoKeyCache remembers Jira lookups of verify_keys across reviews. The
// zero value is ready to use.
type todoKeyCache struct {
	mu sync.Mutex
	m  map[string]todoKeyEntry
}

type todoKeyEntry struct {
	state lint.KeyState
	at    time.Time
}

func (c *todoKeyCache) get(key string, now time.Time) (lint.KeyState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || now.Sub(e.at) > todoKeyTTL {
		return lint.KeyUnchecked, false
	}
	return e.state, true
}

func (c *todoKeyCache) put(key string, s lint.KeyState, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= todoKeyCacheMax {
		c.m = map[string]todoKeyEntry{}
	}
	c.m[key] = todoKeyEntry{state: s, at: now}
}

// todoKeyRun checks keys for one review: the cache first, then at most
// config.MaxTodoKeyLookups Jira lookups.
type todoKeyRun struct {
	lf      *Feature
	lookups int
	skipped int
	failed  bool
}

func (k *todoKeyRun) check(ctx context.Context, key string) (lint.KeyState, error) {
	lf := k.lf
	if s, ok := lf.todoKeys.get(key, lf.env.Now()); ok {
		return s, nil
	}
	if k.lookups >= config.MaxTodoKeyLookups {
		k.skipped++
		return lint.KeyUnchecked, nil
	}
	k.lookups++
	_, err := lf.task.Jira.GetIssue(ctx, key)
	switch {
	case err == nil:
		lf.todoKeys.put(key, lint.KeyExists, lf.env.Now())
		return lint.KeyExists, nil
	case task.IssueMissing(err):
		lf.todoKeys.put(key, lint.KeyMissing, lf.env.Now())
		return lint.KeyMissing, nil
	}
	lf.env.Log.Warn("review: todo: jira lookup", "key", key, "err", err)
	if !k.failed {
		k.failed = true
		jobctx.BotProblem(ctx, problems.JiraFailed, "todo: проверка ключей задач в комментариях", key+": "+err.Error())
	}
	return lint.KeyUnchecked, err
}

// todoKeyExample is the key format shown in the thread.
func todoKeyExample(projectKeys []string) string {
	if len(projectKeys) > 0 && projectKeys[0] != "" {
		return projectKeys[0] + "-123"
	}
	return "KEY-123"
}

// todoNoteBody renders the first note of a todo thread (NoteBody for
// tool "todo"). The comment text comes from the repository: escaped.
func todoNoteBody(id string, f lint.Finding, blocking, nonBlocking, outsideDiff bool) string {
	info := f.Todo
	var b strings.Builder
	b.WriteString(marker.Lint(id, f.Tool))
	b.WriteString("\n")
	if blocking {
		b.WriteString(marker.Blocking)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "**%s %s** · %s · %s\n", lintBadge, f.Tool, lint.TodoTitle(f), finding.Badge(f.Severity))
	switch {
	case blocking:
		b.WriteString("\n⛔ **Блокирует мерж.** Сошлитесь на задачу в комментарии и закройте тред.\n")
	case nonBlocking:
		b.WriteString("\n_Не блокирует мерж._\n")
	}
	if outsideDiff {
		fmt.Fprintf(&b, "\n_Файл %s (вне диффа)._\n", mdtext.FileLineRU(f.Path, f.Line))
	}
	b.WriteString("\n> " + mdtext.Inline(info.Comment, 200) + "\n")
	ex := todoKeyExample(info.ProjectKeys)
	if f.Rule == lint.RuleTodoTaskNotFound {
		fmt.Fprintf(&b, "\nКомментарий ссылается на задачу **%s**, но в Jira её нет или у бота нет к ней доступа. Проверьте ключ: отложенная работа должна вести на существующую задачу.\n", info.Key)
	} else {
		fmt.Fprintf(&b, "\nОтложенная работа без задачи теряется. Заведите задачу и укажите её ключ в комментарии:\n\n```text\n%s(%s): …\n```\n", info.Keyword, ex)
		if len(info.ProjectKeys) > 0 {
			keys := make([]string, 0, len(info.ProjectKeys))
			for _, k := range info.ProjectKeys {
				keys = append(keys, "`"+k+"`")
			}
			fmt.Fprintf(&b, "\nФормат ключа: `%s` (проекты Jira: %s). Подойдёт и ссылка на задачу GitLab (`#123`, `group/project#123`) или URL задачи.\n", ex, strings.Join(keys, ", "))
		} else {
			fmt.Fprintf(&b, "\nПодойдёт ключ Jira (`%s`), ссылка на задачу GitLab (`#123`, `group/project#123`) или URL задачи.\n", ex)
		}
	}
	b.WriteString("\n_Результат проверки todo (автоматическая проверка, без участия Claude)._\n")
	return mdtext.NeutralizeQuickActions(b.String())
}
