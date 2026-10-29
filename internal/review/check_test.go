package review_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/limits"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

var (
	chkDataLoss = output.Finding{Path: "cache.go", Line: 2, Severity: "critical", Title: "Data loss", Body: "writes are dropped"}
	chkSlow     = output.Finding{Path: "cache.go", Line: 4, Severity: "minor", Title: "Slow path", Body: "O(n^2)"}
	chkRace     = output.Finding{Path: "cache.go", Line: 5, Severity: "critical", Title: "Race", Body: "map without lock"}
)

func chkID(f output.Finding) string { return finding.ID(f.Path, f.Title) }

func chkFindingDisc(disc string, f output.Finding, blocking bool) core.Discussion {
	body := finding.NoteBodyOpts(chkID(f), f, false, 0, finding.NoteOpts{Blocking: blocking})
	return core.Discussion{ID: disc, Resolvable: true, Notes: []core.Note{
		reviewtest.BotNote(int64(len(disc))*100+int64(f.Line), body, &core.Position{NewPath: f.Path, NewLine: f.Line}),
	}}
}

const chkSummaryGate = "⛔ **Мерж заблокирован:** 4 блокирующих замечания. Исправьте и закройте треды."

// chkDiscussions: summary (note 500) with a gate line, three open findings
// (two blocking), one open compat and one open blocking lint thread, a
// finding the bot already closed and a human thread.
func chkDiscussions() []core.Discussion {
	return []core.Discussion{
		{ID: "sum", Notes: []core.Note{reviewtest.BotNote(500, marker.Summary("abc123")+"\n"+summary.Title+"\n\n"+chkSummaryGate+"\n\nТекст саммари.\n", nil)}},
		chkFindingDisc("f1", chkDataLoss, true),
		chkFindingDisc("f2", chkSlow, false),
		chkFindingDisc("f3", chkRace, true),
		{ID: "c1", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(2, marker.Compat("bbb", "claude")+"\n"+marker.Blocking+"\nBreaking", nil)}},
		{ID: "l1", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(3, marker.Lint("ccc", "gitleaks")+"\n"+marker.Blocking+"\nsecret", nil)}},
		{ID: "old", Resolvable: true, Resolved: true, Notes: []core.Note{reviewtest.BotNote(4, marker.Finding("ddd")+"\n**🟡 Minor** · Old", nil)}},
		{ID: "human", Resolvable: true, Notes: []core.Note{{ID: 5, AuthorID: 7, AuthorUsername: "alice", Body: "вопрос"}}},
	}
}

func chkHarness(t *testing.T) *reviewtest.Harness {
	t.Helper()
	h := reviewtest.New(t)
	h.GL.Discussions = chkDiscussions()
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "abc123", SummaryNoteID: 500}
	return h
}

func checkOut(t *testing.T, vs ...review.CheckVerdict) string {
	t.Helper()
	b, err := json.Marshal(review.CheckOutput{Findings: vs})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func checkJob(discussion string) core.Job {
	j := reviewtest.ManualJob(core.JobCheck)
	j.DiscussionID = discussion
	return j
}

func repliesTo(g *reviewtest.FakeGitLab, disc string) []string {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	var out []string
	for _, r := range g.Replies {
		if r.DiscussionID == disc {
			out = append(out, r.Body)
		}
	}
	return out
}

func TestCheckGeneralClosesFixed(t *testing.T) {
	h := chkHarness(t)
	m := reviewtest.WithModerator(h)
	h.Cfg.Claude.AdvisorModel, h.Cfg.Claude.SearchModel = "opus", "haiku"
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t,
		review.CheckVerdict{ID: chkID(chkDataLoss), Status: "fixed", Comment: "запись теперь сохраняется в cache.go:2"},
		review.CheckVerdict{ID: chkID(chkSlow), Status: "still_open", Comment: "цикл по-прежнему вложенный"},
		review.CheckVerdict{ID: "ffffffffffff", Status: "fixed", Comment: "unknown id"},
		// chkRace: no verdict -> still open
	)}}
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}

	// One Claude run: the dedicated prompt, read-only tools, claude.model
	// without advisor and search subagent.
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("runs %d", len(h.Runner.Reqs))
	}
	req := h.Runner.Reqs[0]
	if req.JSONSchema != review.CheckSchema || req.NoTools || req.BaseDir == "" ||
		req.Model != h.Cfg.Claude.Model || req.Advisor != "" || req.SearchModel != "" || req.MCP != nil ||
		req.Timeout != h.Cfg.Claude.Timeout.D() || req.Dir != "/work" {
		t.Fatalf("request %+v", req)
	}
	for _, want := range []string{"<untrusted_findings>", "id: " + chkID(chkDataLoss), "id: " + chkID(chkSlow), "id: " + chkID(chkRace),
		"file: cache.go:2", "title: Data loss", "writes are dropped\n", "head111111111111", "(3 findings)"} {
		if !strings.Contains(req.Prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, req.Prompt)
		}
	}
	if strings.Contains(req.Prompt, "ddd") || strings.Contains(req.Prompt, "Breaking") {
		t.Fatalf("closed findings and compat threads are not re-checked:\n%s", req.Prompt)
	}
	if !strings.Contains(req.SystemPrompt, `When unsure, answer "still_open"`) || !strings.Contains(req.SystemPrompt, "Never report new findings") ||
		strings.Contains(req.SystemPrompt, "advisor") {
		t.Fatalf("system prompt:\n%s", req.SystemPrompt)
	}

	// Only the fixed finding is resolved (service token) and told so.
	if strings.Join(m.Resolved, ",") != "f1" || len(h.GL.Resolved) != 0 {
		t.Fatalf("resolved %v / %v", m.Resolved, h.GL.Resolved)
	}
	f1 := repliesTo(h.GL, "f1")
	if len(f1) != 1 || !strings.HasPrefix(f1[0], marker.Fixed+"\n✅ Исправлено: запись теперь сохраняется") {
		t.Fatalf("f1 replies %q", f1)
	}
	if len(repliesTo(h.GL, "f2"))+len(repliesTo(h.GL, "f3")) != 0 {
		t.Fatal("still-open findings get no reply")
	}

	// The command reply.
	got := reviewtest.LastReply(t, h.GL)
	if got.DiscussionID != "cmd-disc" {
		t.Fatalf("reply goes to %q", got.DiscussionID)
	}
	for _, want := range []string{
		"Проверил 3 открытых замечания на `head1111`: закрыто 1, остаются 2.",
		"Остаются открытыми:\n- **Slow path** · `cache.go:4` — цикл по-прежнему вложенный\n- **Race** · `cache.go:5`",
		"линтеров и сканеров (1) и несовместимых изменений (1)", "oasdiff", "закроются после следующего пуша или ревью",
	} {
		if !strings.Contains(got.Body, want) {
			t.Fatalf("reply lacks %q:\n%s", want, got.Body)
		}
	}

	// Gate line: Race, compat and lint still block.
	body := h.GL.Notes[500]
	if !strings.Contains(body, "⛔ **Мерж заблокирован:** 3 блокирующих замечания. Исправьте и закройте треды.") ||
		strings.Contains(body, chkSummaryGate) || !strings.Contains(body, "Текст саммари.") {
		t.Fatalf("summary:\n%s", body)
	}

	// Limits: one rerun of the user, the user limits without the cooldown,
	// recorded without the MR key.
	if len(h.Lim.UserCalls) != 1 || h.Lim.UserCalls[0] != core.UsageRerun {
		t.Fatalf("user checks %v", h.Lim.UserCalls)
	}
	if h.Lim.LastLim.MRRerunCooldown != 0 || h.Lim.LastLim.RerunsPerHour != h.Cfg.Limits.UserRerunsPerHour {
		t.Fatalf("user limits %+v", h.Lim.LastLim)
	}
	recs := h.Lim.Records()
	if len(recs) != 1 || recs[0].Kind != core.UsageRerun || recs[0].UserID != reviewtest.UserID || recs[0].MRKey != "" {
		t.Fatalf("usage %+v", recs)
	}
	if h.Repos.Checkouts != 1 || h.Repos.Closed != 1 {
		t.Fatalf("checkouts %d closed %d", h.Repos.Checkouts, h.Repos.Closed)
	}
}

func TestCheckAllFixedClearsGate(t *testing.T) {
	h := chkHarness(t)
	reviewtest.WithModerator(h)
	// Only the findings: no compat or lint threads.
	h.GL.Discussions = h.GL.Discussions[:4]
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t,
		review.CheckVerdict{ID: chkID(chkDataLoss), Status: "fixed", Comment: "a"},
		review.CheckVerdict{ID: chkID(chkSlow), Status: "FIXED", Comment: "b"},
		review.CheckVerdict{ID: chkID(chkRace), Status: "fixed", Comment: "c"},
	)}}
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	got := reviewtest.LastReply(t, h.GL).Body
	if got != "Проверил 3 открытых замечания на `head1111`: закрыто 3, остаются 0." {
		t.Fatalf("reply %q", got)
	}
	if body := h.GL.Notes[500]; !strings.Contains(body, "\n"+summary.GateClearLine+"\n") || strings.Contains(body, "Мерж заблокирован") {
		t.Fatalf("summary:\n%s", body)
	}
}

func TestCheckInFindingThread(t *testing.T) {
	h := chkHarness(t)
	m := reviewtest.WithModerator(h)
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkRace), Status: "fixed", Comment: "добавлен мьютекс"})}}
	if err := h.R().Handle(context.Background(), checkJob("f3")); err != nil {
		t.Fatal(err)
	}
	p := h.Runner.Reqs[0].Prompt
	if !strings.Contains(p, "id: "+chkID(chkRace)) || strings.Contains(p, chkID(chkDataLoss)) || strings.Contains(p, chkID(chkSlow)) ||
		!strings.Contains(p, "(1 findings)") {
		t.Fatalf("prompt must hold only the thread's finding:\n%s", p)
	}
	if strings.Join(m.Resolved, ",") != "f3" {
		t.Fatalf("resolved %v", m.Resolved)
	}
	rs := repliesTo(h.GL, "f3")
	if len(rs) != 2 || !strings.Contains(rs[0], "✅ Исправлено: добавлен мьютекс") ||
		rs[1] != "Проверил 1 открытое замечание на `head1111`: закрыто 1, остаются 0." {
		t.Fatalf("thread replies %q", rs)
	}
	// Data loss, compat and lint still block.
	if body := h.GL.Notes[500]; !strings.Contains(body, "⛔ **Мерж заблокирован:** 3 блокирующих замечания.") {
		t.Fatalf("summary:\n%s", body)
	}
}

func TestCheckInThreadStillOpen(t *testing.T) {
	h := chkHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkSlow), Status: "still_open", Comment: "цикл на месте"})}}
	if err := h.R().Handle(context.Background(), checkJob("f2")); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Resolved) != 0 {
		t.Fatal("nothing resolved")
	}
	rs := repliesTo(h.GL, "f2")
	if len(rs) != 1 || rs[0] != "Проверил 1 открытое замечание на `head1111`: закрыто 0, остаются 1.\n\nОстаются открытыми:\n- **Slow path** · `cache.go:4` — цикл на месте" {
		t.Fatalf("thread replies %q", rs)
	}
	// Gate recounted from the threads (Data loss, Race, compat, lint): the
	// same line, so the summary is not rewritten.
	if len(h.GL.UpdatedNotes) != 0 {
		t.Fatalf("summary updated: %v", h.GL.UpdatedNotes)
	}
}

func TestCheckReporterFallback(t *testing.T) {
	h := chkHarness(t)
	h.GL.ForbidResolve = true // Reporter bot, no service token
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t,
		review.CheckVerdict{ID: chkID(chkDataLoss), Status: "fixed", Comment: "исправлено в cache.go:2"},
		review.CheckVerdict{ID: chkID(chkSlow), Status: "still_open"},
		review.CheckVerdict{ID: chkID(chkRace), Status: "still_open"},
	)}}
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	f1 := repliesTo(h.GL, "f1")
	if len(f1) != 1 || !strings.Contains(f1[0], "✅ Похоже, исправлено: исправлено в cache.go:2") || !strings.Contains(f1[0], "Тред можно закрыть") ||
		!strings.HasPrefix(f1[0], marker.Fixed) {
		t.Fatalf("f1 replies %q", f1)
	}
	got := reviewtest.LastReply(t, h.GL).Body
	if !strings.Contains(got, "закрыто 1, остаются 2. Ждут закрытия человеком: 1") {
		t.Fatalf("reply %q", got)
	}
	// The fixed thread still blocks in GitLab until a human closes it.
	if body := h.GL.Notes[500]; !strings.Contains(body, "⛔ **Мерж заблокирован:** 4 блокирующих замечания (из них 1 исправлено, ждёт закрытия).") {
		t.Fatalf("summary:\n%s", body)
	}
}

func TestCheckResolveErrorKeepsOpen(t *testing.T) {
	h := chkHarness(t)
	h.GL.FailResolveFor = "f1"
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkDataLoss), Status: "fixed", Comment: "x"})}}
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	if f1 := repliesTo(h.GL, "f1"); len(f1) != 1 || !strings.Contains(f1[0], "Не удалось снять тред автоматически") {
		t.Fatalf("f1 replies %q", f1)
	}
	if got := reviewtest.LastReply(t, h.GL).Body; !strings.Contains(got, "закрыто 0, остаются 3.") || !strings.Contains(got, "**Data loss** · `cache.go:2` — "+review.MsgCheckResolveFail) {
		t.Fatalf("reply %q", got)
	}
}

func TestCheckNoOpenFindings(t *testing.T) {
	h := chkHarness(t)
	// Summary, compat, lint, a closed finding: nothing to re-check.
	h.GL.Discussions = append(h.GL.Discussions[:1:1], h.GL.Discussions[4:]...)
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	got := reviewtest.LastReply(t, h.GL).Body
	if !strings.HasPrefix(got, review.MsgCheckNone+"\n\nОткрытые треды линтеров и сканеров (1) и несовместимых изменений (1)") {
		t.Fatalf("reply %q", got)
	}
	if len(h.Runner.Reqs) != 0 || len(h.Lim.Records()) != 0 || len(h.Lim.UserCalls) != 0 || h.Repos.Checkouts != 0 || len(h.GL.UpdatedNotes) != 0 {
		t.Fatal("no open findings: no booking, no checkout, no Claude run, no summary change")
	}

	// Nothing at all: the bare sentence.
	h = reviewtest.New(t)
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	if got := reviewtest.LastReply(t, h.GL).Body; got != review.MsgCheckNone {
		t.Fatalf("reply %q", got)
	}
}

func TestCheckInToolOrClosedThread(t *testing.T) {
	for disc, want := range map[string]string{
		"l1":  "результат линтера или сканера: команда `@reviewbot check` его не перепроверяет",
		"c1":  "несовместимое изменение: команда `@reviewbot check` его не перепроверяет (нужен запуск проверок совместимости (oasdiff))",
		"old": review.MsgCheckThreadClosed,
	} {
		h := chkHarness(t)
		if err := h.R().Handle(context.Background(), checkJob(disc)); err != nil {
			t.Fatal(err)
		}
		if got := reviewtest.LastReply(t, h.GL); got.DiscussionID != disc || !strings.Contains(got.Body, want) {
			t.Fatalf("%s: reply %+v", disc, got)
		}
		if len(h.Runner.Reqs) != 0 || len(h.Lim.UserCalls) != 0 {
			t.Fatalf("%s: no run, no limits", disc)
		}
	}

	// A human thread or an unknown discussion: every open finding.
	for _, disc := range []string{"human", "gone", ""} {
		h := chkHarness(t)
		h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkSlow), Status: "still_open"})}}
		if err := h.R().Handle(context.Background(), checkJob(disc)); err != nil {
			t.Fatal(err)
		}
		if len(h.Runner.Reqs) != 1 || !strings.Contains(h.Runner.Reqs[0].Prompt, "(3 findings)") {
			t.Fatalf("%q: must check every open finding", disc)
		}
	}
}

func TestCheckAccessAndLimits(t *testing.T) {
	// Below Developer.
	h := chkHarness(t)
	h.GL.Access = core.AccessReporter
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	if got := reviewtest.LastReply(t, h.GL).Body; got != env.MsgNoAccess || len(h.Runner.Reqs) != 0 {
		t.Fatalf("reply %q", got)
	}

	// Per-user rerun limit and global limit refuse with the reason.
	for _, deny := range []string{"user", "global"} {
		h := chkHarness(t)
		d := core.Decision{Reason: "Лимит исчерпан."}
		if deny == "user" {
			h.Lim.User = d
		} else {
			h.Lim.Global = d
		}
		if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
			t.Fatal(err)
		}
		if got := reviewtest.LastReply(t, h.GL).Body; got != "⛔ Лимит исчерпан." || len(h.Runner.Reqs) != 0 || h.Repos.Checkouts != 0 {
			t.Fatalf("%s: reply %q", deny, got)
		}
	}

	// Closed or draft MR, disabled repo.
	h = chkHarness(t)
	h.GL.MR.State = "merged"
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	if got := reviewtest.LastReply(t, h.GL).Body; got != review.MsgNotOpened {
		t.Fatalf("reply %q", got)
	}
	h = chkHarness(t)
	h.GL.RepoConfig = "enabled: false\n"
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	if got := reviewtest.LastReply(t, h.GL).Body; got != review.MsgDisabled {
		t.Fatalf("reply %q", got)
	}
}

// With the real limiter: the per-user rerun limit applies, the per-MR
// cooldown neither applies to the check nor starts from it.
func TestCheckRealLimiterCooldown(t *testing.T) {
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := reviewtest.TestNow
	lim := limits.New(db, 0, nil, func() time.Time { return now })
	ctx := context.Background()

	h := chkHarness(t)
	h.Deps.Limiter, h.Runner.Lim = lim, nil
	h.Cfg.Limits.UserRerunsPerHour = 2
	h.Cfg.Limits.MRRerunCooldown = config.Duration(10 * time.Minute)
	ul := core.UserLimits{RerunsPerHour: 2, MRRerunCooldown: 10 * time.Minute}

	// A review of this MR a minute ago: the cooldown is running.
	if err := lim.Record(ctx, core.Usage{UserID: 77, Kind: core.UsageRerun, MRKey: core.MRKey(reviewtest.PID, reviewtest.IID), At: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if d, _ := lim.CheckUser(ctx, reviewtest.UserID, "bob", core.UsageRerun, core.MRKey(reviewtest.PID, reviewtest.IID), ul); d.Allowed {
		t.Fatal("precondition: the cooldown must refuse a review")
	}
	run := func() string {
		h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkSlow), Status: "still_open"})}}
		if err := h.R().Handle(ctx, checkJob("cmd-disc")); err != nil {
			t.Fatal(err)
		}
		return reviewtest.LastReply(t, h.GL).Body
	}
	if got := run(); !strings.HasPrefix(got, "Проверил 3") {
		t.Fatalf("check must ignore the cooldown: %q", got)
	}

	// Fresh MR state for the cooldown question: a check alone does not
	// start the cooldown for a later review of another user.
	db2, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db2.Close() })
	lim2 := limits.New(db2, 0, nil, func() time.Time { return now })
	h.Env().Limiter = lim2
	if got := run(); !strings.HasPrefix(got, "Проверил 3") {
		t.Fatalf("reply %q", got)
	}
	if d, _ := lim2.CheckUser(ctx, 88, "carol", core.UsageRerun, core.MRKey(reviewtest.PID, reviewtest.IID), ul); !d.Allowed {
		t.Fatalf("a check must not start the MR cooldown: %s", d.Reason)
	}
	// The per-user rerun limit: 2 per hour.
	if got := run(); !strings.HasPrefix(got, "Проверил 3") {
		t.Fatalf("reply %q", got)
	}
	if got := run(); !strings.HasPrefix(got, "⛔ Лимит перезапусков исчерпан (2 в час)") {
		t.Fatalf("third check must hit the rerun limit: %q", got)
	}
	if n := len(h.Runner.Reqs); n != 3 {
		t.Fatalf("runs %d", n)
	}
	rem, err := lim2.Remaining(ctx, reviewtest.UserID, "bob", ul)
	if err != nil || rem.Reruns != 0 {
		t.Fatalf("remaining %+v %v", rem, err)
	}

	// No open findings: nothing booked, nothing recorded.
	h.GL.Discussions = h.GL.Discussions[:1]
	before, _ := db2.ListUsage(ctx, core.UsageFilter{})
	if got := run(); got != review.MsgCheckNone {
		t.Fatalf("reply %q", got)
	}
	after, _ := db2.ListUsage(ctx, core.UsageFilter{})
	if len(after) != len(before) {
		t.Fatalf("usage %d -> %d", len(before), len(after))
	}
}

func TestCheckPostponedAndResumed(t *testing.T) {
	h := chkHarness(t)
	reviewtest.WithModerator(h)
	h.Runner.Resps = []reviewtest.RunResp{{Err: &core.ErrClaudeRateLimited{ResetAt: reviewtest.TestNow.Add(time.Hour), Message: "limit"}}}
	h.Env().Limiter = queue.WrapLimiter(h.Lim) // marks the queue run charged
	run := &reviewtest.QueueRun{ForJob: checkJob("cmd-disc")}
	err := h.R().Handle(run.Attempt(context.Background()), checkJob("cmd-disc"))
	rl, ok := core.AsRetryLater(err)
	if !ok || !rl.At.Equal(reviewtest.TestNow.Add(time.Hour)) {
		t.Fatalf("err %v", err)
	}
	if got := reviewtest.LastReply(t, h.GL).Body; got != "⏳ Перепроверка отложена до 13:00: "+review.ReasonClaudeLimit {
		t.Fatalf("reply %q", got)
	}
	if len(h.GL.UpdatedNotes) != 0 {
		t.Fatal("a postponed check does not touch the summary")
	}

	// The retry skips the access check and the per-user limits.
	h.GL.Access = core.AccessGuest
	h.Lim.User = core.Decision{Reason: "no"}
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkDataLoss), Status: "fixed", Comment: "ok"})}}
	if err := h.R().Handle(run.Attempt(context.Background()), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	if got := reviewtest.LastReply(t, h.GL).Body; !strings.HasPrefix(got, "Проверил 3 открытых замечания на `head1111`: закрыто 1, остаются 2.") {
		t.Fatalf("reply %q", got)
	}
	if len(h.Lim.UserCalls) != 1 {
		t.Fatalf("user limits checked again: %v", h.Lim.UserCalls)
	}
}

func TestCheckInvalidOutput(t *testing.T) {
	h := chkHarness(t)
	svc := problems.New(problems.Options{Store: &reviewtest.ProbStore{Rows: map[string]*problems.Record{}}, Now: func() time.Time { return reviewtest.TestNow }})
	ctx, run := svc.Start(context.Background(), checkJob("cmd-disc"))
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: "not json"},
		{Out: checkOut(t, review.CheckVerdict{ID: "unknown", Status: "fixed"})}, // no verdict for a known id
	}
	err := h.R().Handle(ctx, checkJob("cmd-disc"))
	var inv *env.InvalidOutputError
	if err == nil || !errors.As(err, &inv) {
		t.Fatalf("err %v", err)
	}
	if got := reviewtest.LastReply(t, h.GL).Body; got != fmt.Sprintf(review.MsgCheckFailed, review.ReasonInvalidJSON) {
		t.Fatalf("reply %q", got)
	}
	if len(h.Runner.Reqs) != 2 || !strings.Contains(h.Runner.Reqs[1].Prompt, "previous output was invalid") {
		t.Fatal("one retry with the nudge")
	}
	found := false
	for _, p := range run.List() {
		found = found || p.Kind == problems.InvalidOutput
	}
	if !found {
		t.Fatalf("problems %+v", run.List())
	}
	if len(h.GL.Resolved)+len(h.GL.UpdatedNotes) != 0 {
		t.Fatal("nothing applied")
	}

	// Other runner errors: the internal error reply.
	h = chkHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Err: fmt.Errorf("boom")}}
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err == nil {
		t.Fatal("want error")
	}
	if got := reviewtest.LastReply(t, h.GL).Body; got != fmt.Sprintf(review.MsgCheckFailed, review.ReasonInternal) {
		t.Fatalf("reply %q", got)
	}
}

func TestCheckRecordsOutcomeFixed(t *testing.T) {
	for _, reporter := range []bool{false, true} {
		h, _ := reviewtest.OutcomesHarness(t)
		o := reviewtest.PostFinding(t, h)
		h.GL.ForbidResolve = reporter
		h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: o.FindingID, Status: "fixed", Comment: "исправлено"})}}
		if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
			t.Fatal(err)
		}
		if got := reviewtest.Outcomes(t, h)[0]; got.Status != learn.OutcomeFixed {
			t.Fatalf("reporter=%v: status %q", reporter, got.Status)
		}
	}
}

func TestGateLineHelpers(t *testing.T) {
	if summary.GateLine(0, 3) != summary.GateClearLine {
		t.Fatal("clear")
	}
	if got := summary.GateLine(1, 0); got != "⛔ **Мерж заблокирован:** 1 блокирующее замечание. Исправьте и закройте треды." {
		t.Fatalf("%q", got)
	}
	if got := summary.GateLine(2, 5); got != "⛔ **Мерж заблокирован:** 2 блокирующих замечания (из них 2 исправлены, ждут закрытия). Исправьте и закройте треды." {
		t.Fatalf("%q", got)
	}
	// summary.Render keeps its gate output.
	sum := summary.Render(summary.Data{Gate: true, BlockingOpen: 2, FixedPending: 1, Output: &output.ReviewOutput{}})
	if !strings.Contains(sum, "\n\n⛔ **Мерж заблокирован:** 2 блокирующих замечания (из них 1 исправлено, ждёт закрытия). Исправьте и закройте треды.\n\n") {
		t.Fatalf("summary:\n%s", sum)
	}

	body := "m\n## 🤖 Ревью\n\n" + summary.GateClearLine + "\n\nx"
	out, ok := summary.SetGateLine(body, summary.GateLine(1, 0))
	if !ok || !strings.Contains(out, "\n\n⛔ **Мерж заблокирован:** 1 блокирующее") || strings.Contains(out, summary.GateClearLine) {
		t.Fatalf("%v %q", ok, out)
	}
	if _, ok := summary.SetGateLine("m\n## 🤖 Ревью\n\nтекст", summary.GateClearLine); ok {
		t.Fatal("no gate line: not changed")
	}
}

func TestCheckSummaryWithoutGateUntouched(t *testing.T) {
	h := chkHarness(t)
	reviewtest.WithModerator(h)
	h.GL.Discussions[0].Notes[0].Body = marker.Summary("abc123") + "\n" + summary.Title + "\n\n⏳ Ревью отложено до 13:00: лимит\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkDataLoss), Status: "fixed", Comment: "x"})}}
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.UpdatedNotes) != 0 {
		t.Fatalf("summary without a gate line must not change: %v", h.GL.UpdatedNotes)
	}
}

func TestHelpListsCheck(t *testing.T) {
	h := reviewtest.New(t)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	if got := reviewtest.LastReply(t, h.GL).Body; !strings.Contains(got, "`@reviewbot check` (`@reviewbot перепроверь`)") {
		t.Fatalf("help:\n%s", got)
	}
}
