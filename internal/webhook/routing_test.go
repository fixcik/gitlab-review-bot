package webhook

import (
	"net/http"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
)

// Every command family through the real handler (mention -> ParseCommand ->
// MR commands -> rules -> memory): no command is swallowed by another.
func TestNoteRoutingMatrix(t *testing.T) {
	cases := []struct {
		note string
		want core.JobKind // "" = ignored
	}{
		{"@reviewbot review", core.JobReview},
		{"@reviewbot полное ревью", core.JobFullReview},
		{"@reviewbot review force", core.JobForceReview},
		{"@reviewbot ревью принудительно", core.JobForceReview},
		{"@reviewbot help", core.JobHelp},
		{"@reviewbot", core.JobHelp},
		{"@reviewbot pause", core.JobPause},
		{"@reviewbot пауза: жду релиза", core.JobPause},
		{"@reviewbot продолжить", core.JobResume},
		{"@reviewbot игнорировать", core.JobIgnore},
		{"@reviewbot закрой треды", core.JobResolveAll},
		{"@reviewbot конфиг", core.JobConfig},
		{"@reviewbot check", core.JobCheck},
		{"@reviewbot перепроверь", core.JobCheck},
		{"@reviewbot проверь тесты", core.JobChat},
		{"@reviewbot запомни: не требуй godoc у тестов", core.JobRules},
		{"@reviewbot забудь #3", core.JobRules},
		{"@reviewbot правила", core.JobRules},
		{"@reviewbot правило #3 для всех", core.JobRules},
		{"@reviewbot память", core.JobMemory},
		{"@reviewbot память очистить тесты", core.JobMemory},
		{"@reviewbot память запомни тесты: make test", core.JobMemory},
		{"@reviewbot memory add test: go test ./...", core.JobMemory},
		{"@reviewbot почему здесь мьютекс?", core.JobChat},
		{"@reviewbot pause the world and explain", core.JobChat},
		{"@reviewbot запомни", core.JobChat},
		// Another user with a longer name, and an e-mail-like text: not the bot.
		{"@reviewbot2 review", ""},
		{"mail me at x@reviewbot.example.com", ""},
		// The static alias of older versions is not a mention any more.
		{"@bot review", ""},
	}
	for _, tc := range cases {
		t.Run(tc.note, func(t *testing.T) {
			rec := &recorder{}
			h := NewHandler(Options{
				Secret:  secret,
				Enqueue: rec.enqueue,
				Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", Mention: "@reviewbot", Now: func() time.Time { return fixedNow }, Groups: []string{"group"}, LearnReplies: false}),
			})
			w := do(h, http.MethodPost, "Note Hook", secret, noteHook("bob", tc.note, false, "MergeRequest"))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			switch {
			case tc.want == "" && len(rec.jobs) != 0:
				t.Fatalf("enqueued %+v", rec.jobs)
			case tc.want != "" && (len(rec.jobs) != 1 || rec.jobs[0].Kind != tc.want):
				t.Fatalf("jobs %+v, want %s", rec.jobs, tc.want)
			}
		})
	}
}

// The bot's own notes, system notes and projects outside the groups never
// produce jobs (commands and thread replies alike).
func TestNoteFiltersApplyToEveryKind(t *testing.T) {
	for _, note := range []string{"@reviewbot review", "@reviewbot pause", "@reviewbot память", "@reviewbot правила", "просто ответ в треде"} {
		rec := &recorder{}
		h := NewHandler(Options{Secret: secret, Enqueue: rec.enqueue, Router: events.NewRouter(events.Options{BotUsername: "reviewbot", Groups: []string{"other"}, LearnReplies: true})})
		do(h, http.MethodPost, "Note Hook", secret, noteHook("bob", note, false, "MergeRequest"))
		if len(rec.jobs) != 0 {
			t.Errorf("%q outside groups enqueued %+v", note, rec.jobs)
		}
		rec = &recorder{}
		h = NewHandler(Options{Secret: secret, Enqueue: rec.enqueue, Router: events.NewRouter(events.Options{BotUsername: "reviewbot", LearnReplies: true})})
		do(h, http.MethodPost, "Note Hook", secret, noteHook("ReviewBot", note, false, "MergeRequest"))
		do(h, http.MethodPost, "Note Hook", secret, noteHook("bob", note, true, "MergeRequest"))
		if len(rec.jobs) != 0 {
			t.Errorf("%q own/system note enqueued %+v", note, rec.jobs)
		}
	}
	// A plain threaded reply becomes a learning job only when enabled.
	rec := &recorder{}
	h := NewHandler(Options{Secret: secret, Enqueue: rec.enqueue, Router: events.NewRouter(events.Options{BotUsername: "reviewbot", LearnReplies: true})})
	do(h, http.MethodPost, "Note Hook", secret, noteHook("bob", "это не баг: шлюз делает ретраи", false, "MergeRequest"))
	if len(rec.jobs) != 1 || rec.jobs[0].Kind != core.JobThreadReply {
		t.Fatalf("thread reply: %+v", rec.jobs)
	}
}
