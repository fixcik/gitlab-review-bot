package events

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// The webhook and the poller go through the same Router: identical decisions.
func TestRouterReact(t *testing.T) {
	r := NewRouter(Options{BotUsername: "reviewbot", ReactThanks: true, ReactAck: true})
	n := NoteEvent{ProjectID: 7, ProjectPath: "team/api", IID: 3, NoteID: 9, DiscussionID: "d", UserID: 1, Username: "alice"}
	for body, want := range map[string]core.JobKind{
		"@reviewbot спасибо":           core.JobReact,
		"@reviewbot thx 🙏":             core.JobReact,
		"@reviewbot почему?":           core.JobChat,
		"@reviewbot fixed":             core.JobChat,
		"спасибо":                      core.JobThreadReply,
		"не согласен, это нужно для X": "",
	} {
		e := n
		e.Body, e.Threaded = body, true
		job, why := r.Note(e)
		switch {
		case want == "" && job != nil:
			t.Errorf("%q: got %+v", body, job)
		case want != "" && (job == nil || job.Kind != want):
			t.Errorf("%q: got %+v (%s), want %s", body, job, why, want)
		}
	}
	// The bot's own "thanks" never becomes a job.
	e := n
	e.Username, e.Body, e.Threaded = "reviewbot", "спасибо", true
	if job, _ := r.Note(e); job != nil {
		t.Errorf("own note: %+v", job)
	}
}
