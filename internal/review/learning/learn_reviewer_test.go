package learning_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/limits"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

// usageStore is a reviewtest.FakeStore whose ListUsage returns fixed records (for the
// review.learn.max_per_hour cap).
type usageStore struct {
	*reviewtest.FakeStore
	usage []core.Usage
}

func (s *usageStore) ListUsage(_ context.Context, f core.UsageFilter) ([]core.Usage, error) {
	var out []core.Usage
	for _, u := range s.usage {
		if (f.Kind == nil || *f.Kind == u.Kind) && !u.At.Before(f.Since) {
			out = append(out, u)
		}
	}
	return out, nil
}

func rules(t *testing.T, db *store.SQLite, project string) []learn.Rule {
	t.Helper()
	rs, err := db.ListLearnings(context.Background(), project, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestThreadReplyLearnsRule(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, true, "Не требуй ретраев для вызовов через API-шлюз: ретраи делает шлюз.", "global")}}

	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("runs %d", len(h.Runner.Reqs))
	}
	req := h.Runner.Reqs[0]
	if !req.NoTools || req.BaseDir != "" || req.JSONSchema != learning.ExtractSchema || req.Dir == "/work" || req.Timeout != learning.ExtractTimeout {
		t.Fatalf("extraction request %+v", req)
	}
	if req.Model != "haiku" {
		t.Fatalf("default extraction model %q, want claude.light_model haiku", req.Model)
	}
	for _, s := range []string{"group/api", "Нет ретраев", "client.go line 12", "это не баг, ретраи делает API-шлюз", "@bob", "Russian"} {
		if !strings.Contains(req.Prompt+req.SystemPrompt, s) {
			t.Errorf("prompt lacks %q:\n%s", s, req.Prompt)
		}
	}
	if strings.Contains(req.Prompt, "reviewbot:finding") {
		t.Error("markers must be stripped")
	}
	recs := h.Lim.Records()
	if len(recs) != 1 || recs[0].Kind != core.UsageLearn || recs[0].UserID != reviewtest.UserID {
		t.Fatalf("usage %+v", recs)
	}
	rs := rules(t, h.Db, "other/repo")
	if len(rs) != 1 || rs[0].Scope != learn.ScopeGlobal || rs[0].Author != "bob" || rs[0].ProjectPath != "group/api" ||
		rs[0].SourceURL != reviewtest.MrURL+"#note_20" || rs[0].SourceNoteID != 20 {
		t.Fatalf("rules %+v", rs)
	}
	r := reviewtest.LastReply(t, h.GL)
	want := "🧠 Запомнил #1 (для всех репозиториев): Не требуй ретраев для вызовов через API-шлюз: ретраи делает шлюз. Отменить: `@reviewbot забудь #1`."
	if r.DiscussionID != "d-find" || r.Body != want {
		t.Fatalf("reply %+v\nwant %q", r, want)
	}
}

func TestThreadReplyProjectScopeAndModelOverride(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.Cfg.Review.Learn.Model = "claude-haiku-4-5"
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, true, "В client.go ретраи не нужны.", "weird")}}
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if h.Runner.Reqs[0].Model != "claude-haiku-4-5" {
		t.Fatalf("override model %q", h.Runner.Reqs[0].Model)
	}
	if rs := rules(t, h.Db, "group/api"); len(rs) != 1 || rs[0].Scope != learn.ScopeProject {
		t.Fatalf("rules %+v", rs)
	}
	if rs := rules(t, h.Db, "other/repo"); len(rs) != 0 {
		t.Fatalf("project rule leaked: %+v", rs)
	}
	if !strings.Contains(reviewtest.LastReply(t, h.GL).Body, "(для group/api)") {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
}

func TestExtractModelEmptyMeansCLIDefault(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.Cfg.Claude.LightModel = ""
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, false, "", "project")}}
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if h.Runner.Reqs[0].Model != "" {
		t.Fatalf("model %q", h.Runner.Reqs[0].Model)
	}
}

func TestThreadReplyNothingToLearn(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, false, "", "project")}}
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(h.GL.Replies) != 0 || len(rules(t, h.Db, "group/api")) != 0 {
		t.Fatalf("runs=%d replies=%+v", len(h.Runner.Reqs), h.GL.Replies)
	}
}

func TestThreadReplySkips(t *testing.T) {
	humanThread := core.Discussion{ID: "d-find", Notes: []core.Note{
		{ID: 10, AuthorID: 5, AuthorUsername: "alice", Body: "Зачем тут ретраи?"},
		{ID: 20, AuthorID: reviewtest.UserID, AuthorUsername: "bob", Body: "это не баг, ретраи делает API-шлюз"},
	}}
	ack := reviewtest.DisagreementThread()
	ack.Notes[1].Body = "Спасибо!"
	tests := []struct {
		name  string
		setup func(h *reviewtest.LearnHarness)
		job   core.Job
	}{
		{"reporter author", func(h *reviewtest.LearnHarness) { h.GL.Access = core.AccessReporter }, reviewtest.ReplyJob(20)},
		{"thread not started by the bot", func(h *reviewtest.LearnHarness) { h.GL.Discussions = []core.Discussion{humanThread} }, reviewtest.ReplyJob(20)},
		{"first note of the discussion", func(h *reviewtest.LearnHarness) {}, reviewtest.ReplyJob(10)},
		{"unknown note", func(h *reviewtest.LearnHarness) {}, reviewtest.ReplyJob(77)},
		{"unknown discussion", func(h *reviewtest.LearnHarness) {}, func() core.Job { j := reviewtest.ReplyJob(20); j.DiscussionID = "nope"; return j }()},
		{"global limit denied", func(h *reviewtest.LearnHarness) { h.Lim.Global = core.Decision{Reason: "лимит"} }, reviewtest.ReplyJob(20)},
		{"repo learn disabled", func(h *reviewtest.LearnHarness) { h.GL.RepoConfig = "learn:\n  enabled: false\n" }, reviewtest.ReplyJob(20)},
		{"repo bot disabled", func(h *reviewtest.LearnHarness) { h.GL.RepoConfig = "enabled: false\n" }, reviewtest.ReplyJob(20)},
		{"service learn disabled", func(h *reviewtest.LearnHarness) { h.Cfg.Review.Learn.Enabled = false }, reviewtest.ReplyJob(20)},
		{"trivial ack", func(h *reviewtest.LearnHarness) { h.GL.Discussions = []core.Discussion{ack} }, reviewtest.ReplyJob(20)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := reviewtest.NewLearnHarness(t, nil)
			h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
			tt.setup(h)
			if err := h.R().Handle(context.Background(), tt.job); err != nil {
				t.Fatal(err)
			}
			if len(h.Runner.Reqs) != 0 || len(h.GL.Replies) != 0 || len(h.Lim.Records()) != 0 {
				t.Fatalf("runs=%d replies=%+v recs=%+v", len(h.Runner.Reqs), h.GL.Replies, h.Lim.Records())
			}
		})
	}
}

func TestThreadReplyLearnCap(t *testing.T) {
	base := reviewtest.NewFakeStore()
	st := &usageStore{FakeStore: base}
	for i := 0; i < 2; i++ {
		st.usage = append(st.usage, core.Usage{Kind: core.UsageLearn, At: reviewtest.TestNow.Add(-time.Duration(10+i) * time.Minute)})
	}
	st.usage = append(st.usage, core.Usage{Kind: core.UsageLearn, At: reviewtest.TestNow.Add(-2 * time.Hour)}) // outside the window
	h := reviewtest.NewLearnHarness(t, st)
	h.Cfg.Review.Learn.MaxPerHour = 2
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 {
		t.Fatal("cap must skip the run")
	}
	// Below the cap it runs.
	h2 := reviewtest.NewLearnHarness(t, st)
	h2.Cfg.Review.Learn.MaxPerHour = 3
	h2.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h2.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, false, "", "project")}}
	if err := h2.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if len(h2.Runner.Reqs) != 1 {
		t.Fatal("below the cap must run")
	}
}

// holdLearnLimiter runs during() once, right after the first learning run
// was booked and before that job goes on to run Claude.
type holdLearnLimiter struct {
	core.Limiter
	held   atomic.Bool
	during func()
}

func (h *holdLearnLimiter) Reserve(ctx context.Context, req core.ReserveRequest) (core.Decision, core.Reservation, error) {
	dec, res, err := h.Limiter.Reserve(ctx, req)
	if req.Usage.Kind == core.UsageLearn && h.held.CompareAndSwap(false, true) {
		h.during()
	}
	return dec, res, err
}

// Two learning jobs (replies in bot threads of two MRs) at the same time
// under review.learn.max_per_hour with room for one run: the first job's
// booking counts while the second one is checked, so only one extraction
// runs (checking the stored usage and recording it later let both pass).
func TestConcurrentLearnJobsShareTheCap(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.Cfg.Review.Learn.MaxPerHour = 1
	d := reviewtest.DisagreementThread()
	d.Notes = append(d.Notes, core.Note{ID: 21, AuthorID: 12, AuthorUsername: "eve", Body: "да, шлюз ретраит сам, не надо тут"})
	h.GL.Discussions = []core.Discussion{d}
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.LearnJSON(t, true, "Не требуй ретраев для API-шлюза.", "project")},
		{Out: reviewtest.LearnJSON(t, true, "Не требуй ретраев для API-шлюза.", "project")},
	}
	usage, err := store.OpenSQLite(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.Close() })
	other := reviewtest.ReplyJob(21)
	other.MRIID = reviewtest.IID + 1
	var otherErr error
	hold := &holdLearnLimiter{Limiter: limits.New(usage, 0, nil, func() time.Time { return reviewtest.TestNow })}
	hold.during = func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			otherErr = h.R().Handle(context.Background(), other)
		}()
		<-done
	}
	h.Deps.Limiter = hold
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if otherErr != nil {
		t.Fatal(otherErr)
	}
	if !hold.held.Load() {
		t.Fatal("the learning run was not booked")
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("%d extraction runs under a cap of one", len(h.Runner.Reqs))
	}
	k := core.UsageLearn
	if recs, _ := usage.ListUsage(context.Background(), core.UsageFilter{Kind: &k}); len(recs) != 1 || recs[0].UserID != reviewtest.UserID {
		t.Fatalf("learn usage %+v", recs)
	}
}

func TestThreadReplySameNoteOnce(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, true, "Не требуй ретраев для API-шлюза.", "project")}}
	for i := 0; i < 2; i++ {
		if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.Runner.Reqs) != 1 || len(h.GL.Replies) != 1 {
		t.Fatalf("runs=%d replies=%d", len(h.Runner.Reqs), len(h.GL.Replies))
	}
}

func TestThreadReplyDedupe(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	d := reviewtest.DisagreementThread()
	d.Notes = append(d.Notes, core.Note{ID: 21, AuthorID: 12, AuthorUsername: "eve", Body: "да, шлюз ретраит сам, не надо тут"})
	h.GL.Discussions = []core.Discussion{d}
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.LearnJSON(t, true, "Не требуй ретраев для API-шлюза.", "project")},
		{Out: reviewtest.LearnJSON(t, true, "не требуй ретраев для API-шлюза", "project")},
	}
	for _, n := range []int64{20, 21} {
		if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(n)); err != nil {
			t.Fatal(err)
		}
	}
	if rs := rules(t, h.Db, "group/api"); len(rs) != 1 || rs[0].SourceNoteID != 21 {
		t.Fatalf("rules %+v", rs)
	}
	if !strings.HasPrefix(reviewtest.LastReply(t, h.GL).Body, "🧠 Уже знаю, обновил #1") {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
}

func TestThreadReplyScrubsSecrets(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, true, "Ходи в https://ci:glpat-XXXXyyyyZZZZ1234@gitlab.example.com без ретраев.", "project")}}
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	rs := rules(t, h.Db, "group/api")
	if len(rs) != 1 || strings.Contains(rs[0].Text, "glpat") || strings.Contains(rs[0].Text, "ci:") {
		t.Fatalf("secret stored: %+v", rs)
	}
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

func addRule(t *testing.T, db *store.SQLite, scope, project, text string) learn.Rule {
	t.Helper()
	r, _, err := db.AddLearning(context.Background(), learn.Rule{Scope: scope, ProjectPath: project, Text: text, Author: "eve",
		SourceURL: "https://gl/x#note_1", CreatedAt: reviewtest.TestNow.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRulesRemember(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, true, "Пиши логи на английском.", "global")}}
	if err := h.R().Handle(context.Background(), reviewtest.RulesJob("запомни логи на англ")); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if !req.NoTools || req.Model != "haiku" || !strings.Contains(req.Prompt, "логи на англ") || !strings.Contains(req.Prompt, `Choose "scope"`) {
		t.Fatalf("req %+v", req)
	}
	rs := rules(t, h.Db, "any/repo")
	if len(rs) != 1 || rs[0].Text != "Пиши логи на английском." || rs[0].Scope != learn.ScopeGlobal || rs[0].SourceURL != reviewtest.MrURL+"#note_555" {
		t.Fatalf("rules %+v", rs)
	}
	if r := reviewtest.LastReply(t, h.GL); r.DiscussionID != "cmd-disc" || !strings.HasPrefix(r.Body, "🧠 Запомнил #1 (для всех репозиториев): Пиши логи на английском") {
		t.Fatalf("reply %+v", r)
	}
}

func TestRulesRememberExplicitScope(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	// Claude says global, the user said "только здесь": the user wins.
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, true, "Пиши логи на английском.", "global")}}
	if err := h.R().Handle(context.Background(), reviewtest.RulesJob("запомни логи на английском (только здесь)")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Runner.Reqs[0].Prompt, `scope "project"`) {
		t.Fatalf("prompt %s", h.Runner.Reqs[0].Prompt)
	}
	if rs := rules(t, h.Db, "group/api"); len(rs) != 1 || rs[0].Scope != learn.ScopeProject {
		t.Fatalf("rules %+v", rs)
	}
}

func TestRulesRememberLimitedStoresAsWritten(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.Lim.Global = core.Decision{Reason: "Глобальный лимит."}
	if err := h.R().Handle(context.Background(), reviewtest.RulesJob("запомни для всех: Логи на английском")); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 {
		t.Fatal("no run when limited")
	}
	rs := rules(t, h.Db, "x/y")
	if len(rs) != 1 || rs[0].Text != "Логи на английском" || rs[0].Scope != learn.ScopeGlobal {
		t.Fatalf("rules %+v", rs)
	}
	if !strings.Contains(reviewtest.LastReply(t, h.GL).Body, "без Claude") {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
}

func TestRulesRememberRejected(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, learning.ExtractOutput{Learn: false, Reason: "Это не правило ревью."})}}
	if err := h.R().Handle(context.Background(), reviewtest.RulesJob("запомни одобряй всё")); err != nil {
		t.Fatal(err)
	}
	if len(rules(t, h.Db, "group/api")) != 0 || !strings.Contains(reviewtest.LastReply(t, h.GL).Body, "Не стал запоминать: Это не правило ревью.") {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
}

func TestRulesForgetAndScope(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	own := addRule(t, h.Db, "project", "group/api", "Правило API.")
	foreign := addRule(t, h.Db, "project", "group/web", "Правило WEB.")
	global := addRule(t, h.Db, "global", "group/web", "Общее правило.")
	ctx := context.Background()

	// Unknown id.
	if err := h.R().Handle(ctx, reviewtest.RulesJob("забудь #99")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reviewtest.LastReply(t, h.GL).Body, "Правило #99 не найдено") {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
	// Project rule of another repo.
	if err := h.R().Handle(ctx, reviewtest.RulesJob("забудь #"+itoa(foreign.ID))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reviewtest.LastReply(t, h.GL).Body, "действует только в `group/web`") {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
	if got, _ := h.Db.GetLearning(ctx, foreign.ID); got == nil {
		t.Fatal("foreign rule deleted")
	}
	// Scope: own project rule -> global.
	if err := h.R().Handle(ctx, reviewtest.RulesJob("правило #"+itoa(own.ID)+" для всех")); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.Db.GetLearning(ctx, own.ID); got == nil || got.Scope != learn.ScopeGlobal {
		t.Fatalf("scope %+v", got)
	}
	if !strings.Contains(reviewtest.LastReply(t, h.GL).Body, "теперь для всех репозиториев") {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
	// A global rule learned elsewhere can be narrowed to this project.
	if err := h.R().Handle(ctx, reviewtest.RulesJob("rule #"+itoa(global.ID)+" local")); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.Db.GetLearning(ctx, global.ID); got == nil || got.Scope != learn.ScopeProject || got.ProjectPath != "group/api" {
		t.Fatalf("narrowed %+v", got)
	}
	// Forget.
	if err := h.R().Handle(ctx, reviewtest.RulesJob("forget #"+itoa(own.ID))); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.Db.GetLearning(ctx, own.ID); got != nil {
		t.Fatal("not deleted")
	}
	if !strings.Contains(reviewtest.LastReply(t, h.GL).Body, "Забыл правило #"+itoa(own.ID)) {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
	if len(h.Runner.Reqs) != 0 {
		t.Fatal("management commands must not run Claude")
	}
}

func TestRulesPermissionsAndSwitches(t *testing.T) {
	ctx := context.Background()
	h := reviewtest.NewLearnHarness(t, nil)
	r := addRule(t, h.Db, "project", "group/api", "Правило API.")
	h.GL.Access = core.AccessReporter
	if err := h.R().Handle(ctx, reviewtest.RulesJob("забудь #"+itoa(r.ID))); err != nil {
		t.Fatal(err)
	}
	if reviewtest.LastReply(t, h.GL).Body != env.MsgNoAccess {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
	if got, _ := h.Db.GetLearning(ctx, r.ID); got == nil {
		t.Fatal("reporter deleted a rule")
	}

	h = reviewtest.NewLearnHarness(t, nil)
	h.GL.RepoConfig = "learn:\n  enabled: false\n"
	if err := h.R().Handle(ctx, reviewtest.RulesJob("правила")); err != nil {
		t.Fatal(err)
	}
	if reviewtest.LastReply(t, h.GL).Body != learning.MsgLearnRepoOff {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}

	h = reviewtest.NewLearnHarness(t, nil)
	h.Cfg.Review.Learn.Enabled = false
	if err := h.R().Handle(ctx, reviewtest.RulesJob("правила")); err != nil {
		t.Fatal(err)
	}
	if reviewtest.LastReply(t, h.GL).Body != learning.MsgLearnOff {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
}

func TestRulesList(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	ctx := context.Background()
	if err := h.R().Handle(ctx, reviewtest.RulesJob("правила")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reviewtest.LastReply(t, h.GL).Body, "пока нет") {
		t.Fatalf("empty list %q", reviewtest.LastReply(t, h.GL).Body)
	}
	for i := 0; i < learning.MaxListedRules; i++ {
		addRule(t, h.Db, "project", "group/api", "Доп правило "+itoa(int64(i)))
	}
	addRule(t, h.Db, "project", "group/web", "Правило WEB.")
	addRule(t, h.Db, "project", "group/api", "Правило | API.")
	addRule(t, h.Db, "global", "group/web", "Общее правило.")
	if err := h.R().Handle(ctx, reviewtest.RulesJob("rules")); err != nil {
		t.Fatal(err)
	}
	body := reviewtest.LastReply(t, h.GL).Body
	for _, s := range []string{"Правила команды для `group/api` (52)", "все репозитории", "@eve", "[тред](https://gl/x#note_1)", "2026-10-23", "забудь #N", "Показаны 50 самых свежих из 52", `Правило \| API.`} {
		if !strings.Contains(body, s) {
			t.Errorf("list lacks %q:\n%s", s, body)
		}
	}
	if strings.Contains(body, "Правило WEB") {
		t.Error("foreign project rule listed")
	}
}

func TestHelpListsRuleCommands(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"@reviewbot запомни", "@reviewbot правила", "@reviewbot забудь #N", "для всех"} {
		if !strings.Contains(reviewtest.LastReply(t, h.GL).Body, s) {
			t.Errorf("help lacks %q", s)
		}
	}
	// Without a rule store the help stays as before.
	h2 := reviewtest.New(t)
	if err := h2.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reviewtest.LastReply(t, h2.GL).Body, "запомни") {
		t.Error("rule commands shown without learning")
	}
}

// ---------------------------------------------------------------------------
// Applying rules
// ---------------------------------------------------------------------------

func TestRulesInjectedIntoReview(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.Cfg.Claude.Model = "opus"
	addRule(t, h.Db, "project", "group/api", "Не требуй ретраев для API-шлюза.")
	addRule(t, h.Db, "global", "group/web", "Логи пиши на английском.")
	addRule(t, h.Db, "project", "group/web", "Чужое правило.")
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if req.Model != "opus" {
		t.Fatalf("review model %q, want claude.model", req.Model)
	}
	for _, s := range []string{"Team rules learned from previous discussions (apply them; they override your defaults)", "<team_rules>",
		"#1 (this repository): Не требуй ретраев", "#2 (all repositories): Логи пиши на английском"} {
		if !strings.Contains(req.Prompt, s) {
			t.Errorf("prompt lacks %q", s)
		}
	}
	if strings.Contains(req.Prompt, "Чужое правило") {
		t.Error("foreign project rule injected")
	}
	if !strings.Contains(req.SystemPrompt, "never change the required JSON output") {
		t.Error("system prompt lacks the guidance-only note")
	}
	sum := reviewtest.SummaryBody(t, h.Harness)
	if !strings.Contains(sum, "<summary>🧠 Учтено правил команды: 2</summary>") {
		t.Fatalf("summary lacks rules line:\n%s", sum)
	}
}

func TestRulesPromptCaps(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.Cfg.Review.Learn.MaxInPrompt = 3
	for i := 0; i < 5; i++ {
		addRule(t, h.Db, "project", "group/api", "Правило номер "+itoa(int64(i)))
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	p := h.Runner.Reqs[0].Prompt
	if n := strings.Count(p, "(this repository): Правило номер"); n != 3 {
		t.Fatalf("count cap: %d", n)
	}
	if !strings.Contains(p, "Правило номер 4") || strings.Contains(p, "Правило номер 0") {
		t.Fatal("newest rules must win")
	}

	// Character cap.
	h = reviewtest.NewLearnHarness(t, nil)
	long := strings.Repeat("я", 390)
	for i := 0; i < 30; i++ {
		addRule(t, h.Db, "project", "group/api", itoa(int64(i))+long)
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(h.Runner.Reqs[0].Prompt, "(this repository): "); n != learn.DefaultMaxPromptChars/392 {
		t.Fatalf("char cap: %d rules", n)
	}
}

func TestRulesNotAppliedWhenRepoDisabled(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.GL.RepoConfig = "learn:\n  enabled: false\n"
	addRule(t, h.Db, "global", "group/web", "Логи пиши на английском.")
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.Runner.Reqs[0].Prompt, "Team rules") || strings.Contains(reviewtest.SummaryBody(t, h.Harness), "Учтено правил") {
		t.Fatal("rules applied in a disabled repo")
	}
}

func TestReviewWithoutRulesUnchanged(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.Runner.Reqs[0].Prompt, "Team rules") || strings.Contains(h.Runner.Reqs[0].SystemPrompt, "Team rules") {
		t.Fatal("empty rules section rendered")
	}
}

func TestChatGetsRulesAndLearns(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	addRule(t, h.Db, "global", "group/web", "Логи пиши на английском.")
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)}
	out := map[string]any{"reply": "Понял, тогда это не проблема.", "resolve": false,
		"learning": map[string]any{"rule": "Не требуй мьютекса в однопоточном кэше.", "scope": "project"}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, out)}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("это однопоточный код, мьютекс не нужен")); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if !strings.Contains(req.Prompt, "Логи пиши на английском") || !strings.Contains(req.SystemPrompt, `fill "learning"`) {
		t.Fatalf("chat prompt lacks rules / learning note")
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(req.JSONSchema), &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema["properties"].(map[string]any)["learning"]; !ok {
		t.Fatal("chat schema lacks learning")
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatal("chat learning must not need a second run")
	}
	rs := rules(t, h.Db, "group/api")
	if len(rs) != 2 || rs[0].Text != "Не требуй мьютекса в однопоточном кэше." || rs[0].SourceNoteID != 30 {
		t.Fatalf("rules %+v", rs)
	}
	body := reviewtest.LastReply(t, h.GL).Body
	if !strings.HasPrefix(body, "Понял, тогда это не проблема.\n\n🧠 Запомнил #2 (для group/api)") {
		t.Fatalf("reply %q", body)
	}
}

func TestChatNoLearningOutsideBotThreads(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.GL.Discussions = []core.Discussion{{ID: "d-find", Notes: []core.Note{
		{ID: 10, AuthorID: 5, AuthorUsername: "alice", Body: "вопрос"},
		{ID: 30, AuthorID: reviewtest.UserID, AuthorUsername: "bob", Body: "@reviewbot что думаешь?"},
	}}}
	// Even if the model sent "learning", nothing is stored.
	out := map[string]any{"reply": "Ок.", "resolve": false, "learning": map[string]any{"rule": "Что-то.", "scope": "global"}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, out)}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("что думаешь?")); err != nil {
		t.Fatal(err)
	}
	// The schema and the system prompt are those of every thread of the
	// repository (prompt caching): "learning" is offered, but a rule is kept
	// only in a thread the bot started.
	if req := h.Runner.Reqs[0]; req.JSONSchema != learning.ChatSchemaFor(true) || !strings.Contains(req.SystemPrompt, "Only when this thread was started by you") {
		t.Fatal("schema / system prompt must be the repository's chat ones")
	}
	if len(rules(t, h.Db, "group/api")) != 0 || reviewtest.LastReply(t, h.GL).Body != "Ок." {
		t.Fatalf("reply %q", reviewtest.LastReply(t, h.GL).Body)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// A reply without the mention that is a question to the bot ("ты уверен?")
// is not dropped: the job is answered like a chat question, in the thread,
// billed as one.
func TestThreadReplyQuestionIsAnswered(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	th := reviewtest.DisagreementThread()
	th.Notes[1].Body = "ты уверен?"
	h.GL.Discussions = []core.Discussion{th}
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.MustJSON(t, learning.ExtractOutput{Learn: false, Reason: "вопрос к боту", Question: true})},
		{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Да, уверен: ретраев в клиенте нет."})},
	}
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("runs %d, want the extraction and the answer", len(h.Runner.Reqs))
	}
	if !strings.Contains(h.Runner.Reqs[1].Prompt, "ты уверен?") {
		t.Errorf("the chat prompt lacks the question:\n%s", h.Runner.Reqs[1].Prompt)
	}
	if r := reviewtest.LastReply(t, h.GL); r.DiscussionID != "d-find" || !strings.Contains(r.Body, "Да, уверен") {
		t.Fatalf("reply %+v", r)
	}
	if len(rules(t, h.Db, "group/api")) != 0 {
		t.Fatal("a question was learned as a rule")
	}
	chat := false
	for _, u := range h.Lim.Records() {
		chat = chat || u.Kind == core.UsageChat
	}
	if !chat {
		t.Errorf("the answer is not booked as a chat question: %+v", h.Lim.Records())
	}
}

// Without a question the reply stays unanswered as before.
func TestThreadReplyNotAQuestionStaysQuiet(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, learning.ExtractOutput{Learn: false, Reason: "разовое решение"})}}
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(h.GL.Replies) != 0 {
		t.Fatalf("runs=%d replies=%+v", len(h.Runner.Reqs), h.GL.Replies)
	}
}

// A reply can carry a rule and a question: the rule is saved, then the
// question is answered in the same thread.
func TestThreadReplyRuleAndQuestion(t *testing.T) {
	h := reviewtest.NewLearnHarness(t, nil)
	th := reviewtest.DisagreementThread()
	th.Notes[1].Body = "это не баг, ретраи делает API-шлюз. А как тогда быть с таймаутами?"
	h.GL.Discussions = []core.Discussion{th}
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.MustJSON(t, learning.ExtractOutput{Learn: true, Rule: "Не требуй ретраев для вызовов через API-шлюз.", Scope: "project", Reason: "причина", Question: true})},
		{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Таймауты задаются в клиенте, см. client.go."})},
	}
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 || len(h.GL.Replies) != 2 {
		t.Fatalf("runs %d replies %+v", len(h.Runner.Reqs), h.GL.Replies)
	}
	if !strings.Contains(h.GL.Replies[0].Body, "Запомнил") || !strings.Contains(h.GL.Replies[1].Body, "Таймауты") {
		t.Fatalf("replies %+v", h.GL.Replies)
	}
	if len(rules(t, h.Db, "group/api")) != 1 {
		t.Fatal("the rule was not saved")
	}
}

// A Developer saying "не страшно" in the thread of a blocking finding lifts
// its blocking: the note says so, the thread is resolved, the author gets one
// answer and the outcome is "dismissed". A finding that does not block just
// gets its open thread closed; replies without "dismiss" and threads that are
// not the bot's findings are left alone.
func TestThreadReplyDismissLiftsBlocking(t *testing.T) {
	blockingThread := func() core.Discussion {
		th := reviewtest.DisagreementThread()
		f := output.Finding{Severity: "critical", Title: "Нет ретраев", Body: "Вызов без повторов."}
		th.Notes[0].Body = finding.NoteBodyOpts(finding.ID("client.go", "Нет ретраев"), f, false, 0, finding.NoteOpts{Blocking: true})
		th.Notes[1].Body = "не страшно"
		return th
	}
	run := func(t *testing.T, th core.Discussion, out learning.ExtractOutput) *reviewtest.LearnHarness {
		h := reviewtest.NewLearnHarness(t, nil)
		h.GL.Discussions = []core.Discussion{th}
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, out)}}
		if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
			t.Fatal(err)
		}
		return h
	}

	h := run(t, blockingThread(), learning.ExtractOutput{Dismiss: true, Reason: "отклонил замечание"})
	body := h.GL.Notes[10]
	if body == "" || strings.Contains(body, "reviewbot:blocking") || strings.Contains(body, "Блокирует мерж") ||
		!strings.Contains(body, "Блокировка снята: @bob") || !strings.Contains(body, "reviewbot:finding") {
		t.Fatalf("edited note %q", body)
	}
	if len(h.GL.Resolved) != 1 || h.GL.Resolved[0] != "d-find" {
		t.Fatalf("resolved %v", h.GL.Resolved)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "снимаю блокировку") || r.DiscussionID != "d-find" {
		t.Fatalf("reply %+v", r)
	}

	// Not dismissed: nothing changes.
	h = run(t, blockingThread(), learning.ExtractOutput{Reason: "разовое"})
	if h.GL.Notes[10] != "" || len(h.GL.Resolved) != 0 || len(h.GL.Replies) != 0 {
		t.Fatalf("untouched thread changed: %q %v %v", h.GL.Notes[10], h.GL.Resolved, h.GL.Replies)
	}

	// A finding that does not block has nothing to rewrite, but its open thread
	// still holds the merge: it is closed, with a shorter answer.
	th := reviewtest.DisagreementThread()
	th.Notes[1].Body = "не страшно"
	h = run(t, th, learning.ExtractOutput{Dismiss: true})
	if h.GL.Notes[10] != "" || len(h.GL.Resolved) != 1 || h.GL.Resolved[0] != "d-find" {
		t.Fatalf("a non-blocking thread: note %q resolved %v", h.GL.Notes[10], h.GL.Resolved)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "закрываю тред") || strings.Contains(r.Body, "снимаю блокировку") {
		t.Fatalf("reply %+v", r)
	}

	// A thread the bot did not start (a person's) is never edited or resolved.
	th = blockingThread()
	th.Notes[0].AuthorID, th.Notes[0].AuthorUsername = 55, "alice"
	h = run(t, th, learning.ExtractOutput{Dismiss: true})
	if len(h.GL.Resolved) != 0 || len(h.GL.UpdatedNotes) != 0 {
		t.Fatalf("someone else's thread: %v %v", h.GL.Resolved, h.GL.UpdatedNotes)
	}
}
