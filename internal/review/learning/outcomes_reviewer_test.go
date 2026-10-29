package learning_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

const head2 = "head222222222222"

// nextHead moves the MR to a new head for the next review.
func nextHead(h *reviewtest.LearnHarness, sha string) {
	h.GL.Mu.Lock()
	h.GL.MR.DiffRefs.HeadSHA = sha
	h.GL.Mu.Unlock()
}

func TestOutcomeOpenRecordedOnPublish(t *testing.T) {
	h, _ := reviewtest.OutcomesHarness(t)
	o := reviewtest.PostFinding(t, h)
	if o.Status != learn.OutcomeOpen || o.FindingID != finding.ID("cache.go", "Data loss") || o.Path != "cache.go" || o.Line != 2 ||
		o.Severity != "critical" || o.Title != "Data loss" || o.HeadSHAAtPost != "head111111111111" || o.MRIID != reviewtest.IID ||
		o.BodyExcerpt != "writes are dropped" {
		t.Fatalf("outcome %+v", o)
	}
	if o.DiscussionID != h.GL.Discussions[0].ID {
		t.Fatalf("outcome must be bound to its thread: %q vs %q", o.DiscussionID, h.GL.Discussions[0].ID)
	}

	// With a service token, non-blocking findings are resolved right away:
	// recorded as resolved_by_bot.
	h2, _ := reviewtest.OutcomesHarness(t)
	reviewtest.WithModerator(h2.Harness)
	h2.GL.PublishMakesThreads = true
	h2.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{reviewtest.CritFinding, {Path: "cache.go", Line: 4, Severity: "minor", Title: "Slow", Body: "b"}}, nil)}}
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, o := range reviewtest.Outcomes(t, h2) {
		got[o.Title] = o.Status
	}
	if got["Data loss"] != learn.OutcomeOpen || got["Slow"] != learn.OutcomeResolvedByBot {
		t.Fatalf("statuses %v", got)
	}
}

func TestOutcomeFixedViaBotResolve(t *testing.T) {
	for _, reporter := range []bool{false, true} {
		h, _ := reviewtest.OutcomesHarness(t)
		o := reviewtest.PostFinding(t, h)
		h.GL.ForbidResolve = reporter // Reporter bot: asks a human to close
		nextHead(h, head2)
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, []output.ResolvedFinding{{ID: o.FindingID, Comment: "добавлена запись"}})}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		if got := reviewtest.Outcomes(t, h)[0]; got.Status != learn.OutcomeFixed {
			t.Fatalf("reporter=%v: status %q", reporter, got.Status)
		}
	}
}

func TestOutcomeHumanResolvedClassified(t *testing.T) {
	for _, tc := range []struct {
		name       string
		changed    bool
		resolvedBy int64
		want       string
	}{
		{"unchanged code -> dismissed", false, reviewtest.UserID, learn.OutcomeDismissed},
		{"changed code -> fixed", true, reviewtest.UserID, learn.OutcomeFixed},
		{"resolved_by not reported -> by lines", false, 0, learn.OutcomeDismissed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, lr := reviewtest.OutcomesHarness(t)
			o := reviewtest.PostFinding(t, h)
			h.GL.Mu.Lock()
			h.GL.Discussions[0].Resolved = true
			h.GL.Discussions[0].Notes[0].ResolvedByID = tc.resolvedBy
			h.GL.Mu.Unlock()
			lr.Changed = tc.changed
			nextHead(h, head2)
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if got := reviewtest.Outcomes(t, h)[0]; got.Status != tc.want {
				t.Fatalf("status %q, want %q", got.Status, tc.want)
			}
			want := fmt.Sprintf("%s cache.go 1-5", o.HeadSHAAtPost) // line 2 ± 3, clamped at 1
			if len(lr.Calls) != 1 || lr.Calls[0] != want {
				t.Fatalf("LinesChanged calls %v, want %q", lr.Calls, want)
			}
		})
	}
}

func TestOutcomeBotFixedReplyThenHumanClose(t *testing.T) {
	h, lr := reviewtest.OutcomesHarness(t)
	reviewtest.PostFinding(t, h)
	h.GL.Mu.Lock()
	d := &h.GL.Discussions[0]
	d.Resolved = true
	d.Notes = append(d.Notes, reviewtest.BotNote(9999, marker.Fixed+"\n✅ Похоже, исправлено.", nil))
	h.GL.Mu.Unlock()
	nextHead(h, head2)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := reviewtest.Outcomes(t, h)[0]; got.Status != learn.OutcomeFixed || len(lr.Calls) != 0 {
		t.Fatalf("status %q calls %v", got.Status, lr.Calls)
	}
}

// seedDismissed stores n dismissed, unused outcomes of group/api.
func seedDismissed(t *testing.T, h *reviewtest.LearnHarness, n int) []int64 {
	t.Helper()
	var ids []int64
	for i := 0; i < n; i++ {
		id, err := h.Db.RecordOutcome(context.Background(), learn.Outcome{
			ProjectPath: "group/api", MRIID: 3, FindingID: fmt.Sprintf("f%d", i), Title: fmt.Sprintf("Нет godoc у функции F%d", i),
			Severity: "nit", Path: fmt.Sprintf("internal/x%d.go", i), Line: 10, BodyExcerpt: "Добавьте комментарий.", Status: learn.OutcomeDismissed,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func synthJSON(t *testing.T, rules ...learning.ExtractOutput) string {
	return reviewtest.MustJSON(t, learning.SynthOutput{Rules: rules})
}

func TestSynthesisAtThreshold(t *testing.T) {
	h, _ := reviewtest.OutcomesHarness(t)
	runs := reviewtest.Metered(h.Harness)
	seedDismissed(t, h, 3)
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOut(t, nil, nil)},
		{Out: synthJSON(t,
			learning.ExtractOutput{Rule: "Не требуй godoc у функций в internal/.", Scope: "project", Reason: "3 закрыто"},
			learning.ExtractOutput{Rule: "  ", Scope: "global"}, // empty -> skipped
		)},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("runs %d", len(h.Runner.Reqs))
	}
	req := h.Runner.Reqs[1]
	if !req.NoTools || req.BaseDir != "" || req.JSONSchema != learning.SynthSchema || req.Model != "haiku" || req.Dir == "/work" {
		t.Fatalf("synthesis request %+v", req)
	}
	for _, s := range []string{"Нет godoc у функции F0", "Нет godoc у функции F2", "internal/x1.go:10", "[nit]", "group/api", "<untrusted_findings>"} {
		if !strings.Contains(req.Prompt, s) {
			t.Errorf("synthesis prompt lacks %q:\n%s", s, req.Prompt)
		}
	}
	// Usage: charged as learning, to nobody.
	recs := h.Lim.Records()
	if len(recs) != 2 || recs[1].Kind != core.UsageLearn || recs[1].UserID != 0 {
		t.Fatalf("usage %+v", recs)
	}
	if len(runs.Runs) != 2 || runs.Runs[1].Kind != meter.KindLearning || runs.Runs[1].Username != "" || runs.Runs[0].Username != "alice" {
		t.Fatalf("meter attribution %+v", runs.Runs)
	}
	rs := rules(t, h.Db, "group/api")
	if len(rs) != 1 || rs[0].Text != "Не требуй godoc у функций в internal/." || rs[0].Scope != learn.ScopeProject || rs[0].SourceURL != reviewtest.MrURL {
		t.Fatalf("rules %+v", rs)
	}
	if left, _ := h.Db.ListOutcomes(context.Background(), learn.OutcomeFilter{ProjectPath: "group/api", Status: learn.OutcomeDismissed, Unused: true}); len(left) != 0 {
		t.Fatalf("outcomes must be marked used: %+v", left)
	}
	for _, o := range reviewtest.Outcomes(t, h) {
		if o.RuleID != rs[0].ID || o.UsedAt.IsZero() {
			t.Fatalf("outcome not linked to the rule: %+v", o)
		}
	}
	want := fmt.Sprintf("🧠 Новые правила из закрытых без правок замечаний: #%d Не требуй godoc у функций в internal/, отменить: `@reviewbot забудь #%d`", rs[0].ID, rs[0].ID)
	if sum := reviewtest.SummaryBody(t, h.Harness); !strings.Contains(sum, want) {
		t.Fatalf("summary lacks announcement %q:\n%s", want, sum)
	}

	// The next summary of the MR announces it again; the one after does not.
	for i, sha := range []string{head2, "head333333333333"} {
		nextHead(h, sha)
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		sum := h.GL.Notes[h.GL.CreatedNotes[len(h.GL.CreatedNotes)-1]]
		if has := strings.Contains(sum, "🧠 Новые правила"); has != (i == 0) {
			t.Fatalf("review %d: announcement shown=%v:\n%s", i+2, has, sum)
		}
	}
	if len(h.Runner.Reqs) != 4 {
		t.Fatalf("no new synthesis expected: %d runs", len(h.Runner.Reqs))
	}

	// "@bot правила" names the source.
	h.Runner.Resps = nil
	if err := h.R().Handle(context.Background(), reviewtest.RulesJob("правила")); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "[закрыто без правок: 3 замечания]("+reviewtest.MrURL+")") {
		t.Fatalf("rules list source:\n%s", r.Body)
	}
}

func TestSynthesisBelowThresholdAndConfigurable(t *testing.T) {
	h, _ := reviewtest.OutcomesHarness(t)
	seedDismissed(t, h, 2)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("below threshold: %d runs", len(h.Runner.Reqs))
	}
	h2, _ := reviewtest.OutcomesHarness(t)
	h2.Cfg.Review.Learn.DismissThreshold = 2
	seedDismissed(t, h2, 2)
	h2.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}, {Out: synthJSON(t)}}
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h2.Runner.Reqs) != 2 {
		t.Fatalf("threshold 2: %d runs", len(h2.Runner.Reqs))
	}
	// Nothing learned, outcomes still marked used (not retried forever).
	if len(rules(t, h2.Db, "group/api")) != 0 {
		t.Fatal("no rules expected")
	}
	if left, _ := h2.Db.ListOutcomes(context.Background(), learn.OutcomeFilter{Status: learn.OutcomeDismissed, Unused: true}); len(left) != 0 {
		t.Fatalf("used: %+v", left)
	}
	if strings.Contains(reviewtest.SummaryBody(t, h2.Harness), "🧠 Новые правила") {
		t.Fatal("nothing to announce")
	}
}

func TestSynthesisDeniedByLimits(t *testing.T) {
	// Global limit: the review passes, the synthesis check is denied.
	h, _ := reviewtest.OutcomesHarness(t)
	ids := seedDismissed(t, h, 3)
	h.Lim.GlobalSeq = []core.Decision{{Allowed: true}, {Allowed: false, Reason: "global"}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(h.Lim.Records()) != 1 {
		t.Fatalf("denied: runs %d records %d", len(h.Runner.Reqs), len(h.Lim.Records()))
	}
	if left, _ := h.Db.ListOutcomes(context.Background(), learn.OutcomeFilter{Status: learn.OutcomeDismissed, Unused: true}); len(left) != len(ids) {
		t.Fatalf("outcomes must stay unused: %+v", left)
	}

	// review.learn.max_per_hour reached.
	us := &usageStore{FakeStore: reviewtest.NewFakeStore()}
	for i := 0; i < 2; i++ {
		us.usage = append(us.usage, core.Usage{Kind: core.UsageLearn, At: reviewtest.TestNow.Add(-10 * 60e9)})
	}
	h2 := reviewtest.NewLearnHarness(t, us)
	h2.Cfg.Review.Learn.MaxPerHour = 2
	seedDismissed(t, h2, 3)
	h2.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h2.Runner.Reqs) != 1 {
		t.Fatalf("learn cap: %d runs", len(h2.Runner.Reqs))
	}
}

func TestFromActionsOff(t *testing.T) {
	h, _ := reviewtest.OutcomesHarness(t)
	h.Cfg.Review.Learn.FromActions = false
	seedDismissed(t, h, 5)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{reviewtest.CritFinding}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("no synthesis with from_actions=false: %d runs", len(h.Runner.Reqs))
	}
	if os := reviewtest.Outcomes(t, h); len(os) != 5 { // only the seeded ones
		t.Fatalf("no outcome must be recorded: %d", len(os))
	}
	// Repo learn.enabled: false also switches it off.
	h2, _ := reviewtest.OutcomesHarness(t)
	h2.GL.RepoConfig = "learn:\n  enabled: false\n"
	h2.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{reviewtest.CritFinding}, nil)}}
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(reviewtest.Outcomes(t, h2)) != 0 {
		t.Fatal("repo switch ignored")
	}
}

func TestRulesListStats(t *testing.T) {
	h, _ := reviewtest.OutcomesHarness(t)
	ctx := context.Background()
	if _, _, err := h.Db.AddLearning(ctx, learn.Rule{Scope: "project", ProjectPath: "group/api", Text: "Правило.", CreatedAt: reviewtest.TestNow}); err != nil {
		t.Fatal(err)
	}
	add := func(status string, n int) {
		for i := 0; i < n; i++ {
			if _, err := h.Db.RecordOutcome(ctx, learn.Outcome{ProjectPath: "group/api", MRIID: 1, FindingID: "x", Status: status}); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(learn.OutcomeFixed, 3)
	add(learn.OutcomeDismissed, 1)
	add(learn.OutcomeOpen, 2)
	h.Db.RecordOutcome(ctx, learn.Outcome{ProjectPath: "other/repo", MRIID: 1, FindingID: "x", Status: learn.OutcomeDismissed})
	if err := h.R().Handle(ctx, reviewtest.RulesJob("правила")); err != nil {
		t.Fatal(err)
	}
	body := reviewtest.LastReply(t, h.GL).Body
	if !strings.Contains(body, "📊 **Замечания бота в `group/api` за 30 дней:** исправлено 3 · закрыто без правок 1 · открыто 2") || !strings.Contains(body, "75%") {
		t.Fatalf("stats footer:\n%s", body)
	}

	// Without rules the stats still show; with from_actions=false they do not.
	h2, _ := reviewtest.OutcomesHarness(t)
	h2.Db.RecordOutcome(ctx, learn.Outcome{ProjectPath: "group/api", MRIID: 1, FindingID: "x", Status: learn.OutcomeOpen})
	if err := h2.R().Handle(ctx, reviewtest.RulesJob("правила")); err != nil {
		t.Fatal(err)
	}
	if b := reviewtest.LastReply(t, h2.GL).Body; !strings.Contains(b, "открыто 1") || !strings.Contains(b, "точность (исправлено / (исправлено + закрыто без правок)) —") {
		t.Fatalf("empty list stats:\n%s", b)
	}
	h2.Cfg.Review.Learn.FromActions = false
	if err := h2.R().Handle(ctx, reviewtest.RulesJob("правила")); err != nil {
		t.Fatal(err)
	}
	if b := reviewtest.LastReply(t, h2.GL).Body; strings.Contains(b, "📊") {
		t.Fatalf("stats with from_actions=false:\n%s", b)
	}
}

func TestThreadReplyRuleDismissesOutcome(t *testing.T) {
	h, _ := reviewtest.OutcomesHarness(t)
	ctx := context.Background()
	fid := finding.ID("client.go", "Нет ретраев")
	id, err := h.Db.RecordOutcome(ctx, learn.Outcome{ProjectPath: "group/api", MRIID: reviewtest.IID, FindingID: fid, Status: learn.OutcomeOpen})
	if err != nil {
		t.Fatal(err)
	}
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, true, "Не требуй ретраев для вызовов через API-шлюз.", "global")}}
	if err := h.R().Handle(ctx, reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	for _, o := range reviewtest.Outcomes(t, h) {
		if o.ID == id && (o.Status != learn.OutcomeDismissed || o.RuleID != 1 || o.UsedAt.IsZero()) {
			t.Fatalf("outcome %+v", o)
		}
	}
}

func TestChatResolveMarksOutcome(t *testing.T) {
	h, _ := reviewtest.OutcomesHarness(t)
	ctx := context.Background()
	fid := finding.ID("cache.go", "Race on map")
	h.Db.RecordOutcome(ctx, learn.Outcome{ProjectPath: "group/api", MRIID: reviewtest.IID, FindingID: fid, DiscussionID: "d-find", Status: learn.OutcomeOpen})
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Да.", Resolve: true})}}
	if err := h.R().Handle(ctx, reviewtest.ChatJob("ок?")); err != nil {
		t.Fatal(err)
	}
	if o := reviewtest.Outcomes(t, h)[0]; o.Status != learn.OutcomeResolvedByBot {
		t.Fatalf("status %q", o.Status)
	}
}

// TestOutcomeOtherMRClassified: a thread of another (e.g. merged) MR of the
// project resolved by a human is classified on the next review of any MR,
// comparing lines up to that MR's head.
func TestOutcomeOtherMRClassified(t *testing.T) {
	h, lr := reviewtest.OutcomesHarness(t)
	ctx := context.Background()
	fid := finding.ID("store.go", "Unchecked error")
	id, err := h.Db.RecordOutcome(ctx, learn.Outcome{ProjectPath: "group/api", MRIID: 3, FindingID: fid, Path: "store.go", Line: 40,
		HeadSHAAtPost: "aaaa1111", DiscussionID: "d-other", Status: learn.OutcomeOpen})
	if err != nil {
		t.Fatal(err)
	}
	h.GL.Discussions = []core.Discussion{{ID: "d-other", Resolvable: true, Resolved: true, Notes: []core.Note{
		reviewtest.BotNote(5, finding.NoteBody(fid, output.Finding{Severity: "major", Title: "Unchecked error"}, false, 0), nil),
	}}}
	h.GL.Discussions[0].Notes[0].ResolvedByID = reviewtest.UserID
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(ctx, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(lr.Calls) != 1 || lr.Calls[0] != "aaaa1111 store.go 37-43 head111111111111" {
		t.Fatalf("calls %v", lr.Calls)
	}
	for _, o := range reviewtest.Outcomes(t, h) {
		if o.ID == id && o.Status != learn.OutcomeDismissed {
			t.Fatalf("status %q", o.Status)
		}
	}
}

// svcUserID is the service-token user. It is a person's own (Owner) token,
// so GitLab shows the bot's resolves as resolved by that person.
const svcUserID int64 = 77

// personModerator resolves threads in the fake GitLab as svcUserID.
type personModerator struct {
	reviewtest.FakeModerator
	gl *reviewtest.FakeGitLab
}

func (m *personModerator) ResolveDiscussion(ctx context.Context, p, i int64, id string, resolved bool) error {
	m.gl.Mu.Lock()
	for k := range m.gl.Discussions {
		if m.gl.Discussions[k].ID == id {
			m.gl.Discussions[k].Resolved = resolved
			m.gl.Discussions[k].Notes[0].ResolvedByID = svcUserID
			m.gl.Discussions[k].Notes[0].ResolvedByUsername = "owner"
		}
	}
	m.gl.Mu.Unlock()
	return m.FakeModerator.ResolveDiscussion(ctx, p, i, id, resolved)
}

// TestOutcomeBotResolveWithPersonalServiceToken: the bot resolves threads
// through the service token, which belongs to a real person; resolved_by
// shows that person, but the bot's records say the bot did it -> never
// dismissed, and no line comparison is made.
func TestOutcomeBotResolveWithPersonalServiceToken(t *testing.T) {
	h, lr := reviewtest.OutcomesHarness(t)
	h.Env().Mod = &personModerator{gl: h.GL}
	minor := output.Finding{Path: "cache.go", Line: 4, Severity: "minor", Title: "Slow", Body: "b"}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{reviewtest.CritFinding, minor}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	// Review 2: Claude reports the critical finding fixed; the bot resolves
	// it through the service token.
	nextHead(h, head2)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, []output.ResolvedFinding{{ID: finding.ID(reviewtest.CritFinding.Path, reviewtest.CritFinding.Title), Comment: "ok"}})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	// Review 3: both threads are resolved "by" the person; nothing changes.
	nextHead(h, "head333333333333")
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	h.GL.Mu.Lock()
	for _, d := range h.GL.Discussions {
		if !d.Resolved || d.Notes[0].ResolvedByID != svcUserID {
			t.Fatalf("thread %s should be resolved by the service user: %+v", d.ID, d)
		}
	}
	h.GL.Mu.Unlock()
	got := map[string]string{}
	for _, o := range reviewtest.Outcomes(t, h) {
		got[o.Title] = o.Status
	}
	if got["Data loss"] != learn.OutcomeFixed || got["Slow"] != learn.OutcomeResolvedByBot {
		t.Fatalf("statuses %v", got)
	}
	if len(lr.Calls) != 0 {
		t.Fatalf("bot resolves must not be compared as human closes: %v", lr.Calls)
	}
}

// TestOutcomeServiceTokenUserResolvesManually: the same person resolves a
// thread by hand (the bot did not) -> a human close, classified by lines.
func TestOutcomeServiceTokenUserResolvesManually(t *testing.T) {
	h, lr := reviewtest.OutcomesHarness(t)
	h.Env().Mod = &personModerator{gl: h.GL}
	h.GL.ForbidResolve = true
	reviewtest.PostFinding(t, h) // critical: blocking, the bot leaves it open
	h.GL.Mu.Lock()
	h.GL.Discussions[0].Resolved = true
	h.GL.Discussions[0].Notes[0].ResolvedByID = svcUserID
	h.GL.Mu.Unlock()
	nextHead(h, head2)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := reviewtest.Outcomes(t, h)[0]; got.Status != learn.OutcomeDismissed || len(lr.Calls) != 1 {
		t.Fatalf("status %q calls %v", got.Status, lr.Calls)
	}
}

// "@bot resolve" closes finding threads with the service token. That is the
// bot's own resolve: the outcome must not later count as a human dismissal
// (which would feed "do not report" rule synthesis).
func TestOutcomeResolveAllCommandIsNotDismissal(t *testing.T) {
	h, lr := reviewtest.OutcomesHarness(t)
	m := reviewtest.WithModerator(h.Harness)
	reviewtest.PostFinding(t, h)
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobResolveAll)); err != nil {
		t.Fatal(err)
	}
	if len(m.Resolved) != 1 {
		t.Fatalf("resolved %v", m.Resolved)
	}
	// GitLab now shows the thread as resolved.
	h.GL.Mu.Lock()
	h.GL.Discussions[0].Resolved = true
	h.GL.Mu.Unlock()
	lr.Changed = false // code unchanged: a human close would be "dismissed"
	nextHead(h, head2)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := reviewtest.Outcomes(t, h)[0]; got.Status != learn.OutcomeResolvedByBot {
		t.Fatalf("status %q, want %q", got.Status, learn.OutcomeResolvedByBot)
	}
}
