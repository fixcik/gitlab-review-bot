package review_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestChatReplyAndResolve(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Да, теперь доступ защищён.", Resolve: true})}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("теперь ок?")); err != nil {
		t.Fatal(err)
	}
	if len(h.Lim.UserCalls) != 1 || h.Lim.UserCalls[0] != core.UsageChat {
		t.Fatalf("user checks %v", h.Lim.UserCalls)
	}
	recs := h.Lim.Records()
	if len(recs) != 1 || recs[0].Kind != core.UsageChat || recs[0].UserID != reviewtest.UserID {
		t.Fatalf("records %+v", recs)
	}
	req := h.Runner.Reqs[0]
	if req.JSONSchema != jsonschema.Chat {
		t.Fatal("chat schema not used")
	}
	for _, s := range []string{"теперь ок?", "Я добавил мьютекс", "@bob", "review finding", "cache.go line 2", "Race on map", "(you, the review bot)", "base000000000000..head111111111111"} {
		if !strings.Contains(req.Prompt, s) {
			t.Errorf("prompt lacks %q:\n%s", s, req.Prompt)
		}
	}
	if strings.Contains(req.Prompt, "reviewbot:finding") {
		t.Errorf("markers should be stripped from thread notes")
	}
	if r := reviewtest.LastReply(t, h.GL); r.DiscussionID != "d-find" || r.Body != "Да, теперь доступ защищён." {
		t.Fatalf("reply %+v", r)
	}
	if len(h.GL.Resolved) != 1 || h.GL.Resolved[0] != "d-find" {
		t.Fatalf("resolved %v", h.GL.Resolved)
	}
	if h.Repos.Closed != 1 {
		t.Fatal("checkout not closed")
	}
}

func TestChatResolveOnlyForBotFinding(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.Discussions = []core.Discussion{{ID: "d-find", Notes: []core.Note{
		{ID: 1, AuthorID: reviewtest.UserID, AuthorUsername: "bob", Body: "Why this?"},
	}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Потому что.", Resolve: true})}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("why?")); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Resolved) != 0 {
		t.Fatalf("must not resolve a user thread")
	}
	if !strings.Contains(h.Runner.Reqs[0].Prompt, "not a bot finding") {
		t.Fatal("prompt must say not a finding")
	}
}

func TestChatTopLevelNote(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Ответ."})}}
	job := reviewtest.ChatJob("что делает Foo?")
	job.DiscussionID = ""
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.CreatedNotes) != 1 || h.GL.Notes[h.GL.CreatedNotes[0]] != "@bob Ответ." {
		t.Fatalf("notes %v", h.GL.Notes)
	}
}

func TestChatDenied(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.Access = core.AccessGuest
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("q")); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "Developer") {
		t.Fatalf("reply %+v", r)
	}
	h = reviewtest.New(t)
	h.Lim.User = core.Decision{Reason: "Лимит вопросов исчерпан (20 в час)."}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("q")); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "Лимит вопросов") || len(h.Runner.Reqs) != 0 {
		t.Fatalf("reply %+v", r)
	}
}

func TestChatDraftAnswered(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Draft = true
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "ok"})}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("q")); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatal("chat on drafts must work")
	}
}

func TestChatRateLimited(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.Discussions = []core.Discussion{reviewtest.FindingThread(false)}
	h.Runner.Resps = []reviewtest.RunResp{{Err: &core.ErrClaudeRateLimited{ResetAt: reviewtest.TestNow.Add(2 * time.Hour)}}}
	err := h.R().Handle(context.Background(), reviewtest.ChatJob("q"))
	rl, ok := core.AsRetryLater(err)
	if !ok || !rl.At.Equal(reviewtest.TestNow.Add(2*time.Hour)) {
		t.Fatalf("got %v", err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "14:00") {
		t.Fatalf("reply %+v", r)
	}
}

func TestChatInvalidJSONRetry(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: "plain text"}, {Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "ok"})}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("q")); err != nil {
		t.Fatal(err)
	}
	if len(h.Lim.Records()) != 2 || h.Lim.Records()[1].Kind != core.UsageChat {
		t.Fatalf("records %+v", h.Lim.Records())
	}
}

// The job's booked run counts against its author's hourly window once; a
// further Claude run of the same job (the invalid-JSON retry here; the
// verification and the MCP fallback alike) counts only globally: recorded
// without the user and the MR, like a replay (docs/DESIGN.md "Limits").
func TestChatRetryCountedOnceForUser(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: "plain text"}, {Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "ok"})}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("q")); err != nil {
		t.Fatal(err)
	}
	recs := h.Lim.Records()
	if len(recs) != 2 || recs[0].UserID != reviewtest.UserID || recs[0].MRKey == "" ||
		recs[1].UserID != 0 || recs[1].MRKey != "" || recs[1].Kind != core.UsageChat {
		t.Fatalf("records %+v, want the booked run with the user, the retry global only", recs)
	}
}

func TestHelp(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.RepoConfig = "limits:\n  user_chat_per_hour: 3\n"
	h.Lim.Rem = core.Remaining{Reruns: 4, Chat: -1, Global: 10}
	job := reviewtest.ManualJob(core.JobHelp)
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	r := reviewtest.LastReply(t, h.GL)
	for _, s := range []string{"@reviewbot review", "@reviewbot full review", "@reviewbot help", "полное ревью", "перезапуски ревью: 4", "вопросы: без лимита", "общий лимит запусков: 10", "10 мин"} {
		if !strings.Contains(r.Body, s) {
			t.Errorf("help lacks %q:\n%s", s, r.Body)
		}
	}
	if h.Lim.LastLim.ChatPerHour != 3 {
		t.Fatalf("repo limits not used: %+v", h.Lim.LastLim)
	}
	if len(h.Runner.Reqs) != 0 || len(h.Lim.Records()) != 0 {
		t.Fatal("help must not use Claude")
	}
}

func TestHelpCustomMention(t *testing.T) {
	h := reviewtest.New(t)
	h.Deps.BotUsername = "ai-review"    // the mention follows the bot account's username
	h.Cfg.Review.BotMention = "@static" // deprecated, ignored
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "`@ai-review review`") || strings.Contains(r.Body, "@static") {
		t.Fatalf("reply %s", r.Body)
	}
}
