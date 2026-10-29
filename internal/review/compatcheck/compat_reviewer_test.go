package compatcheck_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

func verifyOut(t *testing.T, verdicts ...output.CompatVerdict) string {
	return reviewtest.MustJSON(t, output.CompatVerification{Verdicts: verdicts})
}

var dropColumn = output.BreakingChange{
	Path: "cache.go", Line: 2, Kind: "db", Title: "Удалена колонка users.email",
	Explanation:      "Предыдущая версия сервиса читает users.email при логине.",
	PreviousBehavior: "колонка users.email существует", NewBehavior: "колонки нет",
	SafeAlternative: "сначала перестать читать колонку, удалить её отдельным релизом",
}

func TestCompatNothingFound(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Compat.Calls) != 1 || h.Compat.Calls[0].Base != "base000000000000" || h.Compat.Calls[0].Head != "head111111111111" {
		t.Fatalf("compat checks must run against the merge base: %+v", h.Compat.Calls)
	}
	req := h.Runner.Reqs[0]
	props, required := reviewtest.SchemaProps(t, req.JSONSchema)
	if props["compatibility"] == nil || !strings.Contains(strings.Join(required, ","), "compatibility") {
		t.Fatalf("schema lacks compatibility: %v", required)
	}
	for _, s := range []string{"Backward compatibility (trunk-based development)", "BLOCKS THE MERGE", "expand"} {
		if !strings.Contains(req.SystemPrompt, s) {
			t.Errorf("system prompt lacks %q", s)
		}
	}
	if !strings.Contains(req.Prompt, "## Backward compatibility") {
		t.Errorf("prompt lacks compat section")
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("no verification run expected: %d runs", len(h.Runner.Reqs))
	}
	sum := reviewtest.SummaryBody(t, h)
	gate := strings.Index(sum, "✅ Блокирующих замечаний нет.")
	sec := strings.Index(sum, summary.CompatSectionTitle)
	text := strings.Index(sum, "Добавлен LRU-кэш.")
	if gate < 0 || sec < 0 || !(gate < sec && sec < text) {
		t.Fatalf("compat section must follow the gate line and precede the summary text:\n%s", sum)
	}
	if !strings.Contains(sum, "✅ Несовместимых изменений не найдено.") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestCompatDeterministicBlocksWithoutVerification(t *testing.T) {
	h := reviewtest.New(t)
	h.Compat.Res = &compat.Result{
		Changes: []compat.Change{reviewtest.OasdiffChange},
		Issues:  []compat.Issue{{Source: compat.SourceBuf, Path: "api/a.proto", Kind: compat.IssueUnavailable, Reason: "buf не установлен в sandbox-образе"}},
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(h.Lim.Records()) != 1 {
		t.Fatalf("deterministic results must not be re-verified: runs=%d", len(h.Runner.Reqs))
	}
	// Tool results go to Claude as data.
	if p := h.Runner.Reqs[0].Prompt; !strings.Contains(p, "<untrusted_tool_results>") || !strings.Contains(p, "removed the optional property 'email'") {
		t.Errorf("prompt lacks tool results:\n%s", p)
	}
	th := reviewtest.CompatThreads(h)
	if len(th) != 1 {
		t.Fatalf("published: %+v", h.GL.Published)
	}
	b := th[0].Body
	for _, s := range []string{marker.Blocking, compatcheck.Badge, "Блокирует мерж", "oasdiff", "response-property-removed", "Безопасная альтернатива"} {
		if !strings.Contains(b, s) {
			t.Errorf("thread lacks %q:\n%s", s, b)
		}
	}
	if th[0].Pos == nil || th[0].Pos.NewLine != 3 {
		t.Errorf("must be anchored inline at line 3: %+v", th[0].Pos)
	}
	sum := reviewtest.SummaryBody(t, h)
	for _, s := range []string{
		"⛔ **Мерж заблокирован:** 1 блокирующее замечание",
		"⛔ **Найдено 1 несовместимое изменение**",
		"**oasdiff** · `cache.go:3`",
		"Проверка buf недоступна (`api/a.proto`)",
	} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
}

func TestCompatBlocksRegardlessOfSeverityAndOutsideDiff(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.WithModerator(h)
	h.Cfg.Review.BlockMinSeverity = "critical"
	h.Compat.Res = &compat.Result{Changes: []compat.Change{{Source: compat.SourceBuf, Path: "proto/a.proto", Rule: "FIELD_NO_DELETE", Message: "field deleted"}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	th := reviewtest.CompatThreads(h)
	if len(th) != 1 || th[0].Pos != nil || !strings.Contains(th[0].Body, "`proto/a.proto` (вне диффа)") {
		t.Fatalf("outside the diff it must be a general thread naming the file: %+v", th)
	}
	if len(m.Resolved) != 0 {
		t.Errorf("compat threads must never be auto-resolved as non-blocking: %v", m.Resolved)
	}
}

func TestCompatClaudeConfirmed(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOutCompat(t, []output.BreakingChange{dropColumn}, nil)},
		{Out: verifyOut(t, output.CompatVerdict{Index: 1, Verdict: "confirmed", Reason: "auth.go читает email"})},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("expected review + verification runs, got %d", len(h.Runner.Reqs))
	}
	recs := h.Lim.Records()
	if len(recs) != 2 || recs[1].Kind != core.UsageAuto || recs[1].MRKey != "42!7" {
		t.Fatalf("verification usage must be recorded like the main run: %+v", recs)
	}
	v := h.Runner.Reqs[1]
	if v.JSONSchema != verify.ClaimsSchema || v.NoTools || v.BaseDir == "" || !strings.Contains(v.Prompt, "/base/<path> is the version before the merge request") {
		t.Errorf("verification must read the code with its own schema and /base: %+v", v)
	}
	for _, s := range []string{"REFUTE", "expand phase", "UNTRUSTED"} {
		if !strings.Contains(v.SystemPrompt, s) {
			t.Errorf("verification system prompt lacks %q", s)
		}
	}
	if !strings.Contains(v.Prompt, "<untrusted_claims>") || !strings.Contains(v.Prompt, "Удалена колонка users.email") {
		t.Errorf("verification prompt:\n%s", v.Prompt)
	}
	th := reviewtest.CompatThreads(h)
	if len(th) != 1 {
		t.Fatalf("published: %+v", h.GL.Published)
	}
	b := th[0].Body
	for _, s := range []string{marker.Blocking, compatcheck.Badge, "Удалена колонка users.email", "**Было:**", "**Стало:**", "**Безопасная альтернатива:**", "Подтверждено перепроверкой: auth.go читает email", "миграция БД"} {
		if !strings.Contains(b, s) {
			t.Errorf("thread lacks %q:\n%s", s, b)
		}
	}
	id, src, _ := marker.ParseCompat(b)
	if src != compatcheck.SourceClaude || id != compat.ID(compatcheck.SourceClaude, "cache.go", "Удалена колонка users.email") {
		t.Errorf("marker: %s %s", id, src)
	}
	sum := reviewtest.SummaryBody(t, h)
	for _, s := range []string{"⛔ **Мерж заблокирован:** 1 блокирующее", "**анализ Claude** · `cache.go:2` — Удалена колонка users.email", "Было: колонка users.email существует → Стало: колонки нет"} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
}

func TestCompatClaudeRejected(t *testing.T) {
	h := reviewtest.New(t)
	other := output.BreakingChange{Path: "cache.go", Line: 4, Kind: "api", Title: "Переименовано поле ttl", Explanation: "клиенты шлют ttl"}
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOutCompat(t, []output.BreakingChange{dropColumn, other}, nil)},
		{Out: verifyOut(t,
			output.CompatVerdict{Index: 1, Verdict: "rejected", Reason: "колонка остаётся, это expand-фаза"},
			output.CompatVerdict{Index: 2, Verdict: "confirmed", Reason: "старое имя не принимается"})},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("all items must be verified in ONE run: %d runs", len(h.Runner.Reqs))
	}
	th := reviewtest.CompatThreads(h)
	if len(th) != 1 || !strings.Contains(th[0].Body, "Переименовано поле ttl") {
		t.Fatalf("only the confirmed item is published: %+v", th)
	}
	sum := reviewtest.SummaryBody(t, h)
	for _, s := range []string{"<summary>Отклонено при перепроверке (1)</summary>", "Удалена колонка users.email — колонка остаётся, это expand-фаза", "Найдено 1 несовместимое изменение"} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
}

func TestCompatVerificationDeniedByGlobalLimit(t *testing.T) {
	h := reviewtest.New(t)
	// First CheckGlobal (the review itself) allowed, the second (verification) denied.
	h.Lim.GlobalSeq = []core.Decision{{Allowed: true}, {Allowed: false, Reason: "общий лимит"}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, []output.BreakingChange{dropColumn}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(h.Lim.Records()) != 1 {
		t.Fatalf("no verification run when the global limit denies: runs=%d", len(h.Runner.Reqs))
	}
	th := reviewtest.CompatThreads(h)
	if len(th) != 1 || !strings.Contains(th[0].Body, marker.Blocking) || !strings.Contains(th[0].Body, "Не перепроверено (достигнут общий лимит запусков Claude)") {
		t.Fatalf("unverified item must still be published as blocking: %+v", th)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, "не перепроверено: достигнут общий лимит") || !strings.Contains(sum, "⛔ **Мерж заблокирован:** 1") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestCompatVerificationClaudeLimitPublishesUnverified(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOutCompat(t, []output.BreakingChange{dropColumn}, nil)},
		{Err: &core.ErrClaudeRateLimited{Message: "usage limit"}},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	th := reviewtest.CompatThreads(h)
	if len(th) != 1 || !strings.Contains(th[0].Body, "Не перепроверено (достигнут лимит использования Claude)") {
		t.Fatalf("%+v", th)
	}
}

func TestCompatDedupeAcrossReviews(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.PublishMakesThreads = true
	h.Compat.Res = &compat.Result{Changes: []compat.Change{reviewtest.OasdiffChange}}
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOutCompat(t, []output.BreakingChange{dropColumn}, nil)},
		{Out: verifyOut(t, output.CompatVerdict{Index: 1, Verdict: "confirmed", Reason: "да"})},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(reviewtest.CompatThreads(h)); n != 2 {
		t.Fatalf("first review: %d compat threads", n)
	}
	// Second review: the same tool result (line moved) and the same Claude
	// item: no new threads, no verification of the already-open item.
	h.GL.MR.DiffRefs.HeadSHA = "head2"
	moved := reviewtest.OasdiffChange
	moved.Line = 5
	h.Compat.Res = &compat.Result{Changes: []compat.Change{moved}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, []output.BreakingChange{dropColumn}, nil)}}
	published := len(h.GL.Published)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobFullReview)); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Published) != published {
		t.Fatalf("duplicates published: %+v", h.GL.Published[published:])
	}
	if len(h.Runner.Reqs) != 3 {
		t.Fatalf("an open item must not be re-verified: %d runs", len(h.Runner.Reqs))
	}
	if p := h.Runner.Reqs[2].Prompt; !strings.Contains(p, "Open breaking-change threads") || !strings.Contains(p, "Удалена колонка users.email") {
		t.Errorf("open Claude compat threads must be listed in the prompt:\n%s", p)
	}
	sum := h.GL.Notes[h.GL.CreatedNotes[0]]
	if !strings.Contains(sum, "⛔ **Мерж заблокирован:** 2 блокирующих замечания") || !strings.Contains(sum, "Найдено 2 несовместимых изменения") {
		t.Errorf("open compat threads count in the gate:\n%s", sum)
	}
}

func TestCompatFixedIsResolved(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.WithModerator(h)
	tool := compatcheck.Item{ID: reviewtest.OasdiffChange.ID(), Source: compat.SourceOasdiff, Path: "cache.go", Line: 3, Rule: reviewtest.OasdiffChange.Rule, Title: reviewtest.OasdiffChange.Message}
	cl := compatcheck.ClaudeItem(dropColumn)
	bufItem := compatcheck.Item{ID: compat.ID(compat.SourceBuf, "a.proto", "X\nfp"), Source: compat.SourceBuf, Path: "a.proto", Title: "field deleted"}
	pos := &core.Position{NewPath: "cache.go", OldPath: "cache.go", NewLine: 3}
	h.GL.Discussions = []core.Discussion{
		{ID: "d-tool", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1, compatcheck.NoteBody(tool, false, 0), pos)}},
		{ID: "d-claude", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(2, compatcheck.NoteBody(cl, false, 0), pos)}},
		{ID: "d-buf", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(3, compatcheck.NoteBody(bufItem, true, 0), nil)}},
	}
	// oasdiff ran fine and no longer reports its change; buf was not
	// available this time, so its thread must stay; Claude says fixed.
	h.Compat.Res = &compat.Result{Issues: []compat.Issue{{Source: compat.SourceBuf, Kind: compat.IssueUnavailable, Reason: "нет buf"}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, []output.ResolvedFinding{{ID: cl.ID, Comment: "колонка возвращена"}})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(m.Resolved, ",")
	if got != "d-tool,d-claude" {
		t.Fatalf("resolved %q", got)
	}
	var replies []string
	for _, r := range h.GL.Replies {
		replies = append(replies, r.DiscussionID+": "+r.Body)
	}
	all := strings.Join(replies, "\n")
	if !strings.Contains(all, "d-claude: "+marker.Fixed+"\n✅ Исправлено: колонка возвращена") || !strings.Contains(all, "d-tool: "+marker.Fixed) {
		t.Errorf("replies:\n%s", all)
	}
	sum := reviewtest.SummaryBody(t, h)
	for _, s := range []string{"⛔ **Мерж заблокирован:** 1 блокирующее", "_(найдено ранее, тред открыт)_", "Исправлено с прошлого ревью: 2."} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
}

func TestCompatFixedReporterPath(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.ForbidResolve = true // Reporter bot, no moderator
	tool := compatcheck.Item{ID: reviewtest.OasdiffChange.ID(), Source: compat.SourceOasdiff, Path: "cache.go", Title: "x"}
	h.GL.Discussions = []core.Discussion{{ID: "d-tool", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1, compatcheck.NoteBody(tool, true, 0), nil)}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	r := reviewtest.LastReply(t, h.GL)
	if r.DiscussionID != "d-tool" || !strings.Contains(r.Body, "Тред можно закрыть") || !strings.Contains(r.Body, marker.Fixed) {
		t.Fatalf("reply: %+v", r)
	}
	// The thread is still unresolved in GitLab (no service token), so it
	// still blocks the merge: the gate line must say so.
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "✅ Несовместимых изменений не найдено.") ||
		!strings.Contains(sum, "⛔ **Мерж заблокирован:** 1 блокирующее замечание (из них 1 исправлено, ждёт закрытия)") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestCompatCheckErrorStillReviews(t *testing.T) {
	h := reviewtest.New(t)
	h.Compat.Err = errors.New("git exploded")
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Runner.Reqs[0].Prompt, "deterministic spec checks could not run") {
		t.Errorf("prompt must say the checks failed")
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "Автоматическая проверка спецификаций не выполнена") {
		t.Errorf("summary:\n%s", sum)
	}
}

func TestCompatChatNeverResolves(t *testing.T) {
	h := reviewtest.New(t)
	it := compatcheck.ClaudeItem(dropColumn)
	h.GL.Discussions = []core.Discussion{{ID: "d-c", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1, compatcheck.NoteBody(it, true, 0), nil)}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Это всё ещё несовместимо.", Resolve: true})}}
	job := reviewtest.ChatJob("это точно ломает?")
	job.DiscussionID = "d-c"
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Runner.Reqs[0].Prompt, "backward-incompatible change that blocks the merge") {
		t.Errorf("chat prompt:\n%s", h.Runner.Reqs[0].Prompt)
	}
	if len(h.GL.Resolved) != 0 {
		t.Fatalf("chat must not resolve a compat thread: %v", h.GL.Resolved)
	}
}

func TestCompatHumanResolvedNotReposted(t *testing.T) {
	h := reviewtest.New(t)
	tool := compatcheck.DeterministicItem(reviewtest.OasdiffChange)
	h.GL.Discussions = []core.Discussion{{ID: "d-tool", Resolvable: true, Resolved: true,
		Notes: []core.Note{reviewtest.BotNote(1, compatcheck.NoteBody(tool, true, 0), nil)}}}
	h.Compat.Res = &compat.Result{Changes: []compat.Change{reviewtest.OasdiffChange}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(reviewtest.CompatThreads(h)); n != 0 {
		t.Fatalf("a thread resolved by a human must not be re-posted: %d", n)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, "Тред закрыт вручную") || !strings.Contains(sum, "✅ Блокирующих замечаний нет.") {
		t.Errorf("summary:\n%s", sum)
	}
}
