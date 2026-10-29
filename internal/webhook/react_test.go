package webhook

import (
	"net/http"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
)

func newReactHandler(rec *recorder, learn, thanks, ack bool) http.Handler {
	return NewHandler(Options{
		Secret:  secret,
		Enqueue: rec.enqueue,
		Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", Now: func() time.Time { return fixedNow }, LearnReplies: learn, ReactThanks: thanks, ReactAck: ack}),
	})
}

func TestReactRouting(t *testing.T) {
	tests := []struct {
		name               string
		learn, thanks, ack bool
		note, typ          string
		want               core.JobKind // "" = no job
		question           string
	}{
		// Addressed to the bot.
		{"thanks", true, true, true, "@reviewbot спасибо!", "DiscussionNote", core.JobReact, "спасибо!"},
		{"thanks, mention at the end", true, true, true, "Спасибо!!! @reviewbot", "", core.JobReact, "Спасибо!!!"},
		{"thanks emoji", false, true, true, "@reviewbot 🙏", "", core.JobReact, "🙏"},
		{"thanks off -> chat", true, false, true, "@reviewbot спасибо", "", core.JobChat, "спасибо"},
		{"ack", true, true, true, "@reviewbot ок, поправлю", "DiscussionNote", core.JobReact, "ок, поправлю"},
		{"ack off -> chat", true, true, false, "@reviewbot ок", "DiscussionNote", core.JobChat, "ок"},
		{"fix claim stays chat (re-checked)", true, true, true, "@reviewbot исправил", "DiscussionNote", core.JobChat, "исправил"},
		{"thanks with a question", true, true, true, "@reviewbot спасибо, а почему тут мьютекс?", "", core.JobChat, "спасибо, а почему тут мьютекс?"},
		{"thanks but", true, true, true, "thanks @reviewbot, but the cache is per request", "", core.JobChat, "but the cache is per request"},
		{"question before the mention", true, true, true, "Почему тут мьютекс? @reviewbot спасибо", "", core.JobChat, "спасибо"},
		{"commands are untouched", true, true, true, "@reviewbot review", "", core.JobReview, ""},
		// Plain replies in a thread (the reviewer checks it is the bot's).
		{"reply thanks, learning off", false, true, true, "спасибо", "DiscussionNote", core.JobThreadReply, ""},
		{"reply ack, learning off", false, true, true, "👍", "DiffNote", core.JobThreadReply, ""},
		{"reply fix claim, learning off", false, true, true, "исправил", "DiscussionNote", core.JobThreadReply, ""},
		{"reply other text, learning off", false, true, true, "так задумано", "DiscussionNote", "", ""},
		{"reply thanks, reactions off", false, false, false, "спасибо", "DiscussionNote", "", ""},
		{"reply ack, ack off", false, true, false, "ок", "DiscussionNote", "", ""},
		{"plain comment thanks (no thread)", false, true, true, "спасибо", "", "", ""},
		{"reply, learning on", true, false, false, "спасибо", "DiscussionNote", core.JobThreadReply, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			h := newReactHandler(rec, tt.learn, tt.thanks, tt.ack)
			do(h, http.MethodPost, "Note Hook", secret, noteHookTyped("bob", tt.note, tt.typ, "d1"))
			if tt.want == "" {
				if len(rec.jobs) != 0 {
					t.Fatalf("expected no job, got %+v", rec.jobs)
				}
				return
			}
			if len(rec.jobs) != 1 || rec.jobs[0].Kind != tt.want || rec.jobs[0].Question != tt.question || rec.jobs[0].NoteID != 555 {
				t.Fatalf("jobs %+v, want %s %q", rec.jobs, tt.want, tt.question)
			}
		})
	}
}
