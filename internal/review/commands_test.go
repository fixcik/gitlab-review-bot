package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

func TestMRCommandsRefuseBelowDeveloper(t *testing.T) {
	for _, kind := range []core.JobKind{core.JobPause, core.JobResume, core.JobIgnore, core.JobResolveAll, core.JobConfig} {
		t.Run(string(kind), func(t *testing.T) {
			h := reviewtest.New(t)
			fs := reviewtest.WithFlags(h)
			m := reviewtest.WithModerator(h)
			h.GL.Access = core.AccessReporter
			h.GL.Discussions = []core.Discussion{{ID: "f1", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1, marker.Finding("aaa")+"\nx", nil)}}}
			if err := h.R().Handle(context.Background(), reviewtest.CmdJob(kind)); err != nil {
				t.Fatal(err)
			}
			if r := reviewtest.LastReply(t, h.GL); r.Body != env.MsgNoAccess || r.DiscussionID != "cmd-disc" {
				t.Fatalf("reply %+v", r)
			}
			if fs.IsPaused() || len(m.Resolved) != 0 {
				t.Fatal("refused command must not act")
			}
			if len(h.Runner.Reqs) != 0 || len(h.Lim.Records()) != 0 || len(h.Lim.UserCalls) != 0 {
				t.Fatal("commands must not use Claude or limits")
			}
		})
	}
}

func TestPauseSkipsAutoReviewButManualRuns(t *testing.T) {
	h := reviewtest.New(t)
	fs := reviewtest.WithFlags(h)
	// Existing summary gets the pause line.
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old0000000000000", SummaryNoteID: 500}
	h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(500, marker.Summary("old0000000000000")+"\n"+summary.Title+"\n\nТекст.", nil)}}}

	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobPause)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); r.Body != "⏸ Авторевью на паузе. Продолжить: `@reviewbot resume`" {
		t.Fatalf("reply %q", r.Body)
	}
	if !fs.IsPaused() || fs.Paused["42!7"] != "bob" {
		t.Fatalf("not paused: %+v", fs.Paused)
	}
	if body := h.GL.Notes[500]; !strings.Contains(body, "⏸ Авторевью на паузе (поставил @\u200bbob)") {
		t.Fatalf("summary lacks pause line:\n%s", body)
	}
	// Second pause: already paused.
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobPause)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "уже на паузе") {
		t.Fatalf("reply %q", r.Body)
	}

	// Auto review is skipped.
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || h.Repos.Checkouts != 0 || len(h.Lim.Records()) != 0 {
		t.Fatal("paused MR must not be auto-reviewed")
	}

	// Manual review still runs, and the new summary keeps the pause line.
	h.Repos.Ancestors["old0000000000000"] = true
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatal("manual review must run while paused")
	}
	body := h.GL.Notes[500]
	if strings.Count(body, "⏸ Авторевью на паузе") != 1 || !strings.Contains(body, "Добавлен LRU-кэш.") {
		t.Fatalf("summary after manual review:\n%s", body)
	}
}

func TestResumeClearsAndEnqueues(t *testing.T) {
	h := reviewtest.New(t)
	fs := reviewtest.WithFlags(h)
	var queued []core.Job
	h.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old0000000000000", SummaryNoteID: 500}
	h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(500, marker.Summary("old")+"\n"+summary.Title+"\n\n⏸ Авторевью на паузе (поставил @\u200bbob)\n\nТекст.", nil)}}}
	fs.Paused["42!7"] = "bob"

	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobResume)); err != nil {
		t.Fatal(err)
	}
	if fs.IsPaused() {
		t.Fatal("pause not cleared")
	}
	r := reviewtest.LastReply(t, h.GL)
	if !strings.Contains(r.Body, review.MsgResumed) || !strings.Contains(r.Body, "поставлено в очередь") {
		t.Fatalf("reply %q", r.Body)
	}
	if len(queued) != 1 || queued[0].Kind != core.JobAutoReview || queued[0].MRIID != reviewtest.IID || queued[0].ProjectPath != "group/api" {
		t.Fatalf("queued %+v", queued)
	}
	if body := h.GL.Notes[500]; strings.Contains(body, "на паузе") || !strings.Contains(body, "Текст.") {
		t.Fatalf("pause line not removed:\n%s", body)
	}
	if len(h.Runner.Reqs) != 0 {
		t.Fatal("resume must not run Claude")
	}

	// Not paused anymore: resume again says so.
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobResume)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "и так включено") || len(queued) != 1 {
		t.Fatalf("reply %q queued %d", r.Body, len(queued))
	}
}

func TestResumeWithoutNewCommitsDoesNotEnqueue(t *testing.T) {
	h := reviewtest.New(t)
	fs := reviewtest.WithFlags(h)
	var queued []core.Job
	h.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "head111111111111"}
	fs.Paused["42!7"] = "bob"
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobResume)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); r.Body != review.MsgResumed || len(queued) != 0 {
		t.Fatalf("reply %q queued %v", r.Body, queued)
	}
}

func TestIgnoreMarkerInDescription(t *testing.T) {
	for name, tc := range map[string]struct {
		desc string
		skip bool
	}{
		"en":            {"Fixes stuff.\n\n@reviewbot ignore", true},
		"ru":            {"@ReviewBot игнорировать — WIP", true},
		"mid text":      {"Please @reviewbot, ignore this one.", true},
		"other user":    {"@reviewbot2 ignore", false},
		"no marker":     {"Fixes stuff.", false},
		"other command": {"@reviewbot review please; ignore the docs", false},
		"word prefix":   {"@reviewbot ignored", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := reviewtest.New(t)
			h.GL.MR.Description = tc.desc
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if got := len(h.Runner.Reqs) == 0; got != tc.skip {
				t.Fatalf("skipped = %v, want %v", got, tc.skip)
			}
		})
	}
	// Custom mention.
	h := reviewtest.New(t)
	h.Deps.BotUsername = "ai"
	h.Cfg.Review.BotMention = "@static" // deprecated, ignored
	if !h.R().HasIgnoreMarker("@AI ignore") || h.R().HasIgnoreMarker("@reviewbot ignore") ||
		h.R().HasIgnoreMarker("@static ignore") || h.R().HasIgnoreMarker("@aim ignore") {
		t.Fatal("the marker must follow the bot account's username only")
	}
	// Manual review is not affected by the marker.
	h = reviewtest.New(t)
	h.GL.MR.Description = "@reviewbot ignore"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobFullReview)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatal("manual review must run despite the marker")
	}
}

func TestIgnoreCommandExplains(t *testing.T) {
	h := reviewtest.New(t)
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobIgnore)); err != nil {
		t.Fatal(err)
	}
	r := reviewtest.LastReply(t, h.GL)
	if !strings.Contains(r.Body, "**описание** MR") || !strings.Contains(r.Body, "`@reviewbot ignore`") || strings.Contains(r.Body, "Сейчас маркер") {
		t.Fatalf("reply %q", r.Body)
	}
	h.GL.MR.Description = "@reviewbot ignore"
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobIgnore)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "Сейчас маркер в описании **есть**") {
		t.Fatalf("reply %q", r.Body)
	}
	if len(h.Runner.Reqs) != 0 {
		t.Fatal("no Claude")
	}
}

func resolveDiscussions() []core.Discussion {
	return []core.Discussion{
		{ID: "sum", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(500, marker.Summary("abc123")+"\n"+summary.Title+"\n\n⛔ **Мерж заблокирован:** 2 блокирующих замечания. Исправьте и закройте треды.\n", nil)}},
		{ID: "f-block", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1, marker.Finding("aaa")+"\n"+marker.Blocking+"\n**🔴 Critical** · X", nil)}},
		{ID: "compat", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(2, "<!-- reviewbot:compat id=bbb source=claude -->\n"+marker.Blocking+"\nBreaking", nil)}},
		{ID: "lint", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(3, "<!-- reviewbot:lint id=ccc -->\nlint", nil)}},
		{ID: "done", Resolvable: true, Resolved: true, Notes: []core.Note{reviewtest.BotNote(4, marker.Finding("ddd")+"\nold", nil)}},
		{ID: "human", Resolvable: true, Notes: []core.Note{{ID: 5, AuthorID: 7, AuthorUsername: "alice", Body: "question"}}},
		{ID: "bot-reply", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(6, "@bob ⏸ Авторевью на паузе.", nil)}},
	}
}

func TestResolveAllWithModerator(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.WithModerator(h)
	h.GL.Discussions = resolveDiscussions()
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "abc123", SummaryNoteID: 500}
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobResolveAll)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.Resolved, ",") != "f-block,compat,lint" {
		t.Fatalf("resolved %v", m.Resolved)
	}
	if r := reviewtest.LastReply(t, h.GL); r.Body != "✅ Закрыто тредов бота: 3." {
		t.Fatalf("reply %q", r.Body)
	}
	if body := h.GL.Notes[500]; !strings.Contains(body, summary.GateClearLine) || strings.Contains(body, "Мерж заблокирован") {
		t.Fatalf("summary gate not cleared:\n%s", body)
	}
	if len(h.GL.Resolved) != 0 {
		t.Fatal("must resolve with the service token only")
	}
}

// Without a service token "resolve" closes the bot's own threads with the
// bot's token; a Forbidden answer (an old GitLab) is counted as a failure.
func TestResolveAllWithoutModerator(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.Discussions = resolveDiscussions()
	h.GL.Discussions[0].Notes[0].Body = "<!-- reviewbot:summary sha=not-hex -->\nsummary"
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobResolveAll)); err != nil {
		t.Fatal(err)
	}
	r := reviewtest.LastReply(t, h.GL)
	if !strings.Contains(r.Body, "Закрыто тредов бота: 3") || len(h.GL.Resolved) != 3 {
		t.Fatalf("reply %q resolved %v", r.Body, h.GL.Resolved)
	}

	// An old GitLab refuses a Reporter: nothing closes, the reply says so.
	h = reviewtest.New(t)
	h.GL.Discussions = resolveDiscussions()
	h.GL.Discussions[0].Notes[0].Body = "<!-- reviewbot:summary sha=not-hex -->\nsummary"
	h.GL.ForbidResolve = true
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobResolveAll)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "Не удалось закрыть: 3") || len(h.GL.Resolved) != 0 {
		t.Fatalf("forbidden: reply %q resolved %v", r.Body, h.GL.Resolved)
	}

	// Nothing open.
	h = reviewtest.New(t)
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobResolveAll)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); r.Body != review.MsgResolveNone {
		t.Fatalf("reply %q", r.Body)
	}
}

func TestConfigCommand(t *testing.T) {
	h := reviewtest.New(t)
	fs := reviewtest.WithFlags(h)
	fs.Paused["42!7"] = "eve"
	h.Cfg.GitLab.Token = "glpat-SECRETSECRETSECRET123"
	h.Cfg.GitLab.ServiceToken = "glpat-SERVICESERVICE12345"
	h.Cfg.GitLab.WebhookSecret = "whsecret-xyz"
	h.Cfg.GitLab.URL = "https://user:pass@gitlab.example.com"
	h.Cfg.Jira.Token = "jira-token-abcdef"
	h.Cfg.Jira.ProjectKeys = []string{"SVC"}
	h.Cfg.Review.BlockMinSeverity = "major"
	h.GL.RepoConfig = "language: en\nauto_review: false\ndiagram: false\nignore: [\"docs/**\"]\n" +
		"limits:\n  user_reruns_per_hour: 2\njira:\n  project_keys: [\"PROJ\"]\nlearn:\n  enabled: false\n" +
		"compat:\n  enabled: true\n  openapi: [\"api/*.yaml\"]\ncontext:\n  exclude: [\"legacy/**\"]\nreactions:\n  enabled: false\n"
	h.GL.MR.Description = "@reviewbot ignore"

	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobConfig)); err != nil {
		t.Fatal(err)
	}
	body := reviewtest.LastReply(t, h.GL).Body
	for _, want := range []string{
		"| Настройка | Значение | Источник |",
		"| `language` | `en` (English) | `.reviewbot.yaml` |",
		"| `auto_review` | ⛔ выкл | `.reviewbot.yaml` |",
		"| `limits.user_reruns_per_hour` | 2 | `.reviewbot.yaml` |",
		"| `limits.user_chat_per_hour` | 20 | по умолчанию |",
		"| `block_min_severity` | `major` | сервис |",
		"| обучение (`learn`) | ⛔ выкл | `.reviewbot.yaml` |",
		"| схема изменений (`diagram`) | ⛔ выкл | `.reviewbot.yaml` |",
		"| реакции (`reactions`) | ⛔ выкл | `.reviewbot.yaml` |",
		"`docs/**`", "`api/*.yaml`", "`legacy/**`",
		"| ключи Jira (`project_keys`) | `PROJ` | `.reviewbot.yaml` |",
		"пауза авторевью | ⏸ да, поставил @\u200beve",
		"маркер `ignore` | да",
		"проверка заголовка",
		"обратная совместимость (`compat`) | ✅ вкл",
		"контекст других репозиториев",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	for _, secret := range []string{"glpat-", "SECRET", "SERVICE", "whsecret", "user:pass", "gitlab.example.com", "jira-token"} {
		if strings.Contains(body, secret) {
			t.Errorf("config leaks %q:\n%s", secret, body)
		}
	}
	if len(h.Runner.Reqs) != 0 || len(h.Lim.Records()) != 0 {
		t.Fatal("config must not use Claude or limits")
	}
	if t.Failed() {
		t.Log(body)
	}
}

func TestConfigCommandDefaults(t *testing.T) {
	h := reviewtest.New(t)
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobConfig)); err != nil {
		t.Fatal(err)
	}
	body := reviewtest.LastReply(t, h.GL).Body
	for _, want := range []string{
		"| `language` | `ru` (Russian) | по умолчанию |",
		"| `auto_review` | ✅ вкл | по умолчанию |",
		"| схема изменений (`diagram`) | ✅ вкл | по умолчанию |",
		"| реакции (`reactions`) | ✅ вкл: picked_up `:eyes:`, done `:leftwards_arrow_with_hook:`",
		"| инструменты модели | только чтение файлов (`Read`, `Grep`, `Glob`), код до MR — в `/base`; команды не запускаются | по умолчанию |",
		"пауза авторевью | нет",
		"нет `.reviewbot.yaml`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config lacks %q:\n%s", want, body)
		}
	}
}

func TestHelpListsMRCommands(t *testing.T) {
	h := reviewtest.New(t)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	body := reviewtest.LastReply(t, h.GL).Body
	for _, want := range []string{"`@reviewbot pause`", "`@reviewbot пауза`", "`@reviewbot resume`", "`@reviewbot продолжить`",
		"`@reviewbot resolve`", "`@reviewbot закрой треды`", "`@reviewbot config`", "`@reviewbot конфиг`",
		"`@reviewbot ignore`", "`@reviewbot игнорировать`", "описании MR"} {
		if !strings.Contains(body, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

// The target branch is a user-controlled name: in the "@bot config" reply
// it is one code span that it cannot close ("main`@all`x" would otherwise
// mention everybody), with or without a .reviewbot.yaml on it.
func TestConfigCommandTargetBranchCannotMention(t *testing.T) {
	for _, repoCfg := range []string{"", "language: en\n"} {
		h := reviewtest.New(t)
		h.GL.MR.TargetBranch = "main`@all`x<!-- reviewbot:ci"
		h.GL.RepoConfig, h.GL.RepoConfigRef = repoCfg, h.GL.MR.TargetBranch
		if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobConfig)); err != nil {
			t.Fatal(err)
		}
		body := reviewtest.LastReply(t, h.GL).Body
		if !strings.Contains(body, "`main@allx<!​-- reviewbot:ci`") || mdtext.DefuseMentions(body) != body || strings.Contains(body, "<!--") {
			t.Errorf("config %q, target branch:\n%s", repoCfg, body)
		}
	}
}

// GitLab 19.4 makes a plain MR comment a resolvable thread, and an open one
// blocks the merge ("All threads must be resolved"): after answering a
// service command the bot resolves the thread the command started, with the
// service token; a finding thread the command was typed into stays open (a
// question does too: core.TestKindsClosingTheirThread).
func TestServiceCommandClosesItsThread(t *testing.T) {
	root := func(id int64, author string) []core.Note {
		return []core.Note{{ID: id, AuthorID: 7, AuthorUsername: author, Body: "@reviewbot help"}}
	}
	cases := []struct {
		name  string
		kind  core.JobKind
		disc  core.Discussion
		noMod bool
		want  bool
	}{
		{"help in its own thread", core.JobHelp, core.Discussion{ID: "cmd-disc", Resolvable: true, Notes: root(555, "bob")}, false, true},
		{"config in its own thread", core.JobConfig, core.Discussion{ID: "cmd-disc", Resolvable: true, Notes: root(555, "bob")}, false, true},
		{"typed inside a finding thread", core.JobHelp, core.Discussion{ID: "cmd-disc", Resolvable: true,
			Notes: []core.Note{reviewtest.BotNote(300, "<!-- reviewbot:finding id=x -->", nil), {ID: 555, AuthorID: 7, AuthorUsername: "bob"}}}, false, false},
		{"already resolved", core.JobHelp, core.Discussion{ID: "cmd-disc", Resolvable: true, Resolved: true, Notes: root(555, "bob")}, false, false},
		{"not resolvable", core.JobHelp, core.Discussion{ID: "cmd-disc", Notes: root(555, "bob")}, false, false},
		{"no service token", core.JobHelp, core.Discussion{ID: "cmd-disc", Resolvable: true, Notes: root(555, "bob")}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := reviewtest.New(t)
			h.GL.Discussions = []core.Discussion{c.disc}
			mod := &reviewtest.FakeModerator{}
			if !c.noMod {
				h.Env().Mod = mod
			} else {
				h.Env().Mod = nil
			}
			job := reviewtest.ManualJob(c.kind)
			if err := h.R().Handle(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			got := len(mod.Resolved) == 1 && mod.Resolved[0] == "cmd-disc"
			if got != c.want {
				t.Errorf("resolved %v (service token resolved %v), want %v", got, mod.Resolved, c.want)
			}
			if len(h.GL.Resolved) != 0 {
				t.Errorf("the bot's own token resolved %v", h.GL.Resolved)
			}
		})
	}
}
