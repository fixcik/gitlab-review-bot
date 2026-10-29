package review_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/reactions"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

// fakeAwards is the award emoji API: awards per note (0 = the MR).
type fakeAwards struct {
	mu     sync.Mutex
	awards map[int64][]core.Award
	next   int64
	log    []string // "+note:name" / "-note:name" in call order
}

func (f *fakeAwards) ListAwards(_ context.Context, _, _, note int64) ([]core.Award, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Award(nil), f.awards[note]...), nil
}

func (f *fakeAwards) AddAward(_ context.Context, _, _, note int64, name string) (core.Award, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.awards[note] {
		if a.UserID == reviewtest.BotID && a.Name == name {
			return core.Award{}, fmt.Errorf("gitlab: HTTP 404: has already been taken: %w", core.ErrNotFound)
		}
	}
	f.next++
	a := core.Award{ID: f.next, Name: name, UserID: reviewtest.BotID, Username: reviewtest.BotName}
	f.awards[note] = append(f.awards[note], a)
	f.log = append(f.log, fmt.Sprintf("+%d:%s", note, name))
	return a, nil
}

func (f *fakeAwards) DeleteAward(_ context.Context, _, _, note, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keep []core.Award
	for _, a := range f.awards[note] {
		if a.ID == id {
			f.log = append(f.log, fmt.Sprintf("-%d:%s", note, a.Name))
			continue
		}
		keep = append(keep, a)
	}
	f.awards[note] = keep
	return nil
}

// on lists the awards of a note as "who:name", sorted ("bot" for the bot).
func (f *fakeAwards) on(note int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.awards[note] {
		who := a.Username
		if a.UserID == reviewtest.BotID {
			who = "bot"
		}
		out = append(out, who+":"+a.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func (f *fakeAwards) calls() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.log, " ")
}

type reactHarness struct {
	*reviewtest.Harness
	aw      *fakeAwards
	db      *store.SQLite
	mu      sync.Mutex
	metrics map[string]int
}

func (h *reactHarness) metric(k string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.metrics[k]
}

// newReactHarness is reviewtest.New with reactions on (default config) and a
// SQLite rule store (learning).
func newReactHarness(t *testing.T) *reactHarness {
	t.Helper()
	h := &reactHarness{Harness: reviewtest.New(t), aw: &fakeAwards{awards: map[int64][]core.Award{}, next: 500}, metrics: map[string]int{}}
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h.db = db
	rc := reactions.New(reactions.Options{API: h.aw, BotID: reviewtest.BotID, BotUsername: reviewtest.BotName, Timeout: 2 * time.Second,
		Observe: func(e reactions.Event, res string) {
			h.mu.Lock()
			h.metrics[string(e)+"/"+res]++
			h.mu.Unlock()
		}})
	h.Deps = review.Deps{
		Compat: h.Compat,
		GitLab: h.GL, Repos: h.Repos, Runner: h.Runner, Store: h.St, Limiter: h.Lim, Config: h.Cfg,
		Learnings: db, Outcomes: db, ScratchDir: t.TempDir(),
		BuildIndex:  func([]core.FileDiff) env.DiffIndex { return h.Idx },
		BotUserID:   reviewtest.BotID,
		BotUsername: reviewtest.BotName,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:         func() time.Time { return reviewtest.TestNow },
		Reactions:   rc,
	}
	h.Reset()
	return h
}

// enqueue is what main does: Accepted (👀) in front of the queue.
func (h *reactHarness) enqueue(t *testing.T, job core.Job) {
	t.Helper()
	var got []core.Job
	h.R().Accepted(func(j core.Job) { got = append(got, j) })(job)
	if len(got) != 1 {
		t.Fatalf("Accepted did not pass the job on: %v", got)
	}
}

func TestReactionsHelpLifecycle(t *testing.T) {
	h := newReactHarness(t)
	job := reviewtest.ManualJob(core.JobHelp)
	h.enqueue(t, job)
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(555); got != "bot:leftwards_arrow_with_hook" {
		t.Fatalf("awards %q (calls %s)", got, h.aw.calls())
	}
	if got := h.aw.calls(); got != "+555:eyes -555:eyes +555:leftwards_arrow_with_hook" {
		t.Errorf("calls %s", got)
	}
	if h.metric("picked_up/added") != 1 || h.metric("done/added") != 1 {
		t.Errorf("metrics %v", h.metrics)
	}
}

func TestReactionsHandleWithoutAccepted(t *testing.T) {
	// A job that did not pass Accepted (e.g. re-queued) still gets 👀 first.
	h := newReactHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Ответ."})}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("почему?")); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.calls(); got != "+30:eyes -30:eyes +30:leftwards_arrow_with_hook" {
		t.Errorf("calls %s", got)
	}
}

func TestReactionsChatFailedAndDoneEmpty(t *testing.T) {
	h := newReactHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Err: fmt.Errorf("claude crashed")}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("почему?")); err == nil {
		t.Fatal("want error")
	}
	if got := h.aw.on(30); got != "bot:x" {
		t.Fatalf("failed: %q", got)
	}
	// done: "" only removes 👀.
	h = newReactHarness(t)
	h.Cfg.Review.Reactions.Done = ""
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Ответ."})}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("почему?")); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(30); got != "" || h.aw.calls() != "+30:eyes -30:eyes" {
		t.Fatalf("done off: %q (%s)", got, h.aw.calls())
	}
}

func TestReactionsPostponedThenResumed(t *testing.T) {
	h := newReactHarness(t)
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)}
	h.Runner.Resps = []reviewtest.RunResp{
		{Err: &core.ErrClaudeRateLimited{ResetAt: reviewtest.TestNow.Add(time.Hour)}},
		{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Теперь ок."})},
	}
	job := reviewtest.ChatJob("теперь ок?")
	h.enqueue(t, job)
	err := h.R().Handle(context.Background(), job)
	if _, ok := core.AsRetryLater(err); !ok {
		t.Fatalf("got %v", err)
	}
	if got := h.aw.on(30); got != "bot:hourglass_flowing_sand" {
		t.Fatalf("postponed: %q", got)
	}
	// The queue runs the same job again later: 👀 comes back, then ✅.
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(30); got != "bot:leftwards_arrow_with_hook" {
		t.Fatalf("resumed: %q", got)
	}
	want := "+30:eyes -30:eyes +30:hourglass_flowing_sand -30:hourglass_flowing_sand +30:eyes -30:eyes +30:leftwards_arrow_with_hook"
	if got := h.aw.calls(); got != want {
		t.Errorf("calls\n%s\nwant\n%s", got, want)
	}
}

func TestReactionsDenied(t *testing.T) {
	// No access: 🚫 next to the text reply.
	h := newReactHarness(t)
	h.GL.Access = core.AccessGuest
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobPause)); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(555); got != "bot:no_entry_sign" || !strings.Contains(reviewtest.LastReply(t, h.GL).Body, "Developer") {
		t.Fatalf("no access: %q", got)
	}
	// Per-user limit of a chat question.
	h = newReactHarness(t)
	h.Lim.User = core.Decision{Reason: "Лимит вопросов исчерпан (20 в час)."}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("q")); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(30); got != "bot:no_entry_sign" {
		t.Fatalf("limit: %q", got)
	}
	// Per-user limit of "@bot review".
	h = newReactHarness(t)
	h.Lim.User = core.Decision{Reason: "Лимит перезапусков исчерпан."}
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(555); got != "bot:no_entry_sign" || h.metric("denied/added") != 1 {
		t.Fatalf("review limit: %q %v", got, h.metrics)
	}
}

func TestReactionsDeniedRepoConfig(t *testing.T) {
	// Not opted in (review.require_repo_config, no .reviewbot.yaml): the
	// command gets the "how to connect" reply and 🚫, not ✅.
	for _, kind := range []core.JobKind{core.JobReview, core.JobPause, core.JobConfig} {
		h := newReactHarness(t)
		h.Cfg.Review.RequireRepoConfig = true
		job := reviewtest.ManualJob(kind)
		h.enqueue(t, job)
		if err := h.R().Handle(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		if got := h.aw.on(555); got != "bot:no_entry_sign" || !strings.Contains(reviewtest.LastReply(t, h.GL).Body, "не подключён") {
			t.Errorf("%s: not opted in: %q (calls %s)", kind, got, h.aw.calls())
		}
	}
	// Invalid config on the target branch: 🚫 as well.
	h := newReactHarness(t)
	h.GL.RepoConfig = "languag: ru\n"
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(555); got != "bot:no_entry_sign" {
		t.Errorf("invalid config: %q (calls %s)", got, h.aw.calls())
	}
	// "@bot help" still answers (with the hint) and gets ✅.
	h = newReactHarness(t)
	h.Cfg.Review.RequireRepoConfig = true
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(555); got != "bot:leftwards_arrow_with_hook" {
		t.Errorf("help: %q", got)
	}
}

func reactJob(q string) core.Job {
	j := reviewtest.ManualJob(core.JobReact)
	j.DiscussionID, j.NoteID, j.Question = "d-find", 31, q
	return j
}

func TestReactionsThanksCostsNoRun(t *testing.T) {
	h := newReactHarness(t)
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)}
	if err := h.R().Handle(context.Background(), reactJob("спасибо!")); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(31); got != "bot:heart" {
		t.Fatalf("awards %q", got)
	}
	if len(h.Runner.Reqs) != 0 || len(h.GL.Replies) != 0 || len(h.GL.CreatedNotes) != 0 || len(h.Lim.Records()) != 0 || len(h.Lim.UserCalls) != 0 {
		t.Fatalf("thanks must not run Claude, reply or touch limits: runs=%d replies=%v", len(h.Runner.Reqs), h.GL.Replies)
	}
	if h.metric("thanks/added") != 1 {
		t.Errorf("metrics %v", h.metrics)
	}
}

func TestReactionsAckAddressed(t *testing.T) {
	// "@bot ок, поправлю" in a bot finding thread: 👍, no run.
	h := newReactHarness(t)
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)}
	if err := h.R().Handle(context.Background(), reactJob("ок, поправлю")); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(31); got != "bot:thumbsup" || len(h.Runner.Reqs) != 0 {
		t.Fatalf("awards %q runs %d", got, len(h.Runner.Reqs))
	}
	// Outside a bot remark thread it is a chat question as before (with 👀 … ✅).
	h = newReactHarness(t)
	h.GL.Discussions = []core.Discussion{{ID: "d-find", Notes: []core.Note{{ID: 31, AuthorID: reviewtest.UserID, AuthorUsername: "bob", Body: "@reviewbot ок"}}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Хорошо."})}}
	if err := h.R().Handle(context.Background(), reactJob("ок")); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || reviewtest.LastReply(t, h.GL).Body != "Хорошо." {
		t.Fatalf("fallback chat: runs %d", len(h.Runner.Reqs))
	}
	if got := h.aw.on(31); got != "bot:leftwards_arrow_with_hook" {
		t.Fatalf("fallback lifecycle: %q (%s)", got, h.aw.calls())
	}
}

func TestReactionsRepoDisabled(t *testing.T) {
	// .reviewbot.yaml reactions.enabled: false — no awards at all; a thanks
	// is answered by chat as before reactions existed.
	h := newReactHarness(t)
	h.GL.RepoConfig = "reactions:\n  enabled: false\n"
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Пожалуйста!"})}}
	job := reactJob("спасибо")
	h.enqueue(t, job)
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("fallback chat runs %d", len(h.Runner.Reqs))
	}
	help := reviewtest.ManualJob(core.JobHelp)
	h.enqueue(t, help)
	if err := h.R().Handle(context.Background(), help); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.calls(); got != "" {
		t.Fatalf("awards with reactions off: %s", got)
	}
	if h.metric("picked_up/disabled") != 2 || h.metric("done/disabled") != 2 {
		t.Errorf("metrics %v", h.metrics)
	}
	// Service switch off: nothing is even queued.
	h = newReactHarness(t)
	h.Cfg.Review.Reactions.Enabled = false
	h.enqueue(t, reviewtest.ManualJob(core.JobHelp))
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.calls(); got != "" || len(h.metrics) != 0 {
		t.Fatalf("service off: %s %v", got, h.metrics)
	}
}

func TestReactionsNotForBotsAndIgnoredUsers(t *testing.T) {
	h := newReactHarness(t)
	h.Cfg.Review.Reactions.IgnoreUsers = []string{"renovate-bot"}
	for _, u := range []string{"renovate-bot", "project_42_bot_abc123", reviewtest.BotName} {
		job := reviewtest.ManualJob(core.JobHelp)
		job.Username = u
		if u == reviewtest.BotName {
			job.UserID = reviewtest.BotID
		}
		h.enqueue(t, job)
		if err := h.R().Handle(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.aw.calls(); got != "" {
		t.Fatalf("reacted to %s", got)
	}
}

func TestReactionsHumanAwardsKept(t *testing.T) {
	h := newReactHarness(t)
	h.aw.awards[555] = []core.Award{{ID: 1, Name: "eyes", UserID: 5, Username: "alice"}, {ID: 2, Name: "leftwards_arrow_with_hook", UserID: 6, Username: "carol"}}
	job := reviewtest.ManualJob(core.JobHelp)
	h.enqueue(t, job)
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(555); got != "alice:eyes,bot:leftwards_arrow_with_hook,carol:leftwards_arrow_with_hook" {
		t.Fatalf("awards %q", got)
	}
}

func threadWith(notes ...core.Note) core.Discussion {
	return core.Discussion{ID: "d-find", Resolvable: true, Notes: notes}
}

func TestReactionsThreadReplies(t *testing.T) {
	finding := reviewtest.DisagreementThread().Notes[0]
	human := func(id int64, body string) core.Note {
		return core.Note{ID: id, AuthorID: reviewtest.UserID, AuthorUsername: "bob", Body: body}
	}
	summary := reviewtest.BotNote(10, "<!-- reviewbot:summary sha=abc -->\nСводка", nil)
	cases := []struct {
		name  string
		learn bool
		disc  core.Discussion
		want  string
	}{
		{"thanks after the bot's finding", true, threadWith(finding, human(20, "спасибо!")), "bot:heart"},
		{"thanks, learning off", false, threadWith(finding, human(20, "thx 🙏")), "bot:heart"},
		{"thanks in the summary thread", true, threadWith(summary, human(20, "спасибо")), "bot:heart"},
		{"thanks to a colleague", true, threadWith(finding, human(15, "это из-за шлюза"), human(20, "спасибо")), ""},
		{"ack in a finding thread", true, threadWith(finding, human(20, "ок, поправлю")), "bot:thumbsup"},
		{"fix claim in a finding thread", false, threadWith(finding, human(20, "исправил")), "bot:thumbsup"},
		{"ack in the summary thread", true, threadWith(summary, human(20, "ок")), ""},
		{"thread not started by the bot", true, threadWith(human(5, "вопрос"), human(20, "спасибо")), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t)
			h.Cfg.Review.Learn.Enabled = tc.learn
			h.GL.Discussions = []core.Discussion{tc.disc}
			if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
				t.Fatal(err)
			}
			if got := h.aw.on(20); got != tc.want {
				t.Fatalf("awards %q, want %q", got, tc.want)
			}
			if len(h.Runner.Reqs) != 0 || len(h.GL.Replies) != 0 {
				t.Fatalf("runs %d replies %v", len(h.Runner.Reqs), h.GL.Replies)
			}
		})
	}
}

func TestReactionsLearned(t *testing.T) {
	// A rule learned from a reply: 👀 during the run, then 🧠 (no ✅).
	h := newReactHarness(t)
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, true, "Не требуй ретраев для вызовов через API-шлюз.", "global")}}
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(20); got != "bot:brain" {
		t.Fatalf("awards %q (%s)", got, h.aw.calls())
	}
	if got := h.aw.calls(); got != "+20:eyes -20:eyes +20:brain" {
		t.Errorf("calls %s", got)
	}
	// Nothing learned and nothing asked: 👀 is replaced by 👍, so the author
	// sees the reply was read (no text answer).
	h = newReactHarness(t)
	h.GL.Discussions = []core.Discussion{reviewtest.DisagreementThread()}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, false, "", "project")}}
	if err := h.R().Handle(context.Background(), reviewtest.ReplyJob(20)); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(20); got != "bot:thumbsup" || h.aw.calls() != "+20:eyes -20:eyes +20:thumbsup" {
		t.Fatalf("not learned: %q (%s)", got, h.aw.calls())
	}
	// "@bot запомни …": ✅ and 🧠 on the command.
	h = newReactHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.LearnJSON(t, true, "Логи пиши на английском.", "project")}}
	if err := h.R().Handle(context.Background(), reviewtest.RulesJob("запомни логи на английском")); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(555); got != "bot:brain,bot:leftwards_arrow_with_hook" {
		t.Fatalf("remember: %q", got)
	}
}

func TestReactionsCleanReview(t *testing.T) {
	h := newReactHarness(t)
	h.aw.awards[0] = []core.Award{{ID: 1, Name: "tada", UserID: 5, Username: "alice"}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(0); got != "alice:tada,bot:tada" {
		t.Fatalf("clean review: %q", got)
	}
	// The next review finds something: the bot's 🎉 goes, alice's stays.
	h.Repos.Ancestors["head111111111111"] = true
	h.GL.MR.DiffRefs.HeadSHA = "head222222222222"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{{Path: "cache.go", Line: 2, Severity: "major", Title: "Race", Body: "x"}}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.on(0); got != "alice:tada" {
		t.Fatalf("not clean: %q (%s)", got, h.aw.calls())
	}
	// Off: clean_review "".
	h = newReactHarness(t)
	h.Cfg.Review.Reactions.CleanReview = ""
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := h.aw.calls(); got != "" {
		t.Fatalf("clean_review off: %s", got)
	}
}

// attrRunner records the meter attribution of every Claude run.
type attrRunner struct {
	core.Runner
	mu    sync.Mutex
	kinds []string
}

func (a *attrRunner) Run(ctx context.Context, req core.RunRequest) (*core.RunResult, error) {
	if at, ok := meter.From(ctx); ok {
		a.mu.Lock()
		a.kinds = append(a.kinds, at.Kind)
		a.mu.Unlock()
	}
	return a.Runner.Run(ctx, req)
}

// A thanks that falls back to chat is billed as a question (chat) on
// /usage, not as "other".
func TestReactionsFallbackBilledAsChat(t *testing.T) {
	h := newReactHarness(t)
	h.GL.RepoConfig = "reactions:\n  enabled: false\n"
	ar := &attrRunner{Runner: h.Deps.Runner}
	h.Deps.Runner = ar
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Пожалуйста!"})}}
	job := reactJob("спасибо")
	h.enqueue(t, job)
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ar.kinds, ",") != meter.KindChat {
		t.Fatalf("run kinds %v", ar.kinds)
	}
}

// A command the queue drops on restore (answered or expired while the bot
// was down) loses the bot's 👀 / ⏳: nothing will finish it. Other users'
// awards stay; notes the bot does not react to are left alone.
func TestReactionsDroppedOnRestore(t *testing.T) {
	h := newReactHarness(t)
	job := reviewtest.ManualJob(core.JobReview)
	h.aw.awards[555] = []core.Award{
		{ID: 1, Name: "eyes", UserID: reviewtest.BotID, Username: reviewtest.BotName},
		{ID: 2, Name: "hourglass_flowing_sand", UserID: reviewtest.BotID, Username: reviewtest.BotName},
		{ID: 3, Name: "eyes", UserID: 42, Username: "alice"},
	}
	h.R().Dropped(job, "expired")
	deadline := time.Now().Add(2 * time.Second)
	for h.aw.on(555) != "alice:eyes" && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := h.aw.on(555); got != "alice:eyes" {
		t.Fatalf("awards %q (calls %s)", got, h.aw.calls())
	}

	own := job
	own.UserID, own.Username, own.NoteID = reviewtest.BotID, reviewtest.BotName, 556
	h.aw.awards[556] = []core.Award{{ID: 4, Name: "eyes", UserID: reviewtest.BotID, Username: reviewtest.BotName}}
	h.R().Dropped(own, "answered")
	h.R().Dropped(core.Job{Kind: core.JobRefreshStatus, ProjectID: 1, MRIID: 2}, "expired")
	time.Sleep(20 * time.Millisecond)
	if got := h.aw.on(556); got != "bot:eyes" {
		t.Fatalf("the bot's own note touched: %q", got)
	}
}
