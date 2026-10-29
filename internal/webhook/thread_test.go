package webhook

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
)

func newLearnHandler(rec *recorder, learn bool) http.Handler {
	return NewHandler(Options{
		Secret:  secret,
		Enqueue: rec.enqueue,
		Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", Now: func() time.Time { return fixedNow }, LearnReplies: learn}),
	})
}

// noteHookTyped is noteHook with a configurable object_attributes.type
// ("" -> null) and discussion id.
func noteHookTyped(author, note, typ, discussionID string) string {
	body := noteHook(author, note, false, "MergeRequest")
	t := `null`
	if typ != "" {
		t = `"` + typ + `"`
	}
	body = strings.Replace(body, `"type": "DiscussionNote"`, `"type": `+t, 1)
	return strings.Replace(body, `"discussion_id": "d1scuss10n"`, `"discussion_id": "`+discussionID+`"`, 1)
}

func TestThreadReplyJobs(t *testing.T) {
	tests := []struct {
		name     string
		learn    bool
		author   string
		note     string
		typ      string
		disc     string
		wantKind core.JobKind // "" = no job
	}{
		{"reply in discussion", true, "bob", "это не баг, ретраи делает API-шлюз", "DiscussionNote", "d1", core.JobThreadReply},
		{"reply in diff thread", true, "bob", "так задумано", "DiffNote", "d1", core.JobThreadReply},
		{"plain comment (no thread)", true, "bob", "LGTM", "", "d1", ""},
		{"no discussion id", true, "bob", "так задумано", "DiscussionNote", "", ""},
		{"learning off", false, "bob", "так задумано", "DiscussionNote", "d1", ""},
		{"own note", true, "reviewbot", "🧠 Запомнил #1", "DiscussionNote", "d1", ""},
		{"reply with mention -> chat", true, "bob", "@reviewbot почему?", "DiscussionNote", "d1", core.JobChat},
		{"rule command", true, "bob", "@reviewbot забудь #12", "DiscussionNote", "d1", core.JobRules},
		{"rule command remember", true, "bob", "@reviewbot запомни логи на английском", "", "d1", core.JobRules},
		{"rules list en", true, "bob", "@reviewbot rules", "DiscussionNote", "d1", core.JobRules},
		{"rule command even with learning off goes to handler", false, "bob", "@reviewbot правила", "DiscussionNote", "d1", core.JobRules},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			h := newLearnHandler(rec, tt.learn)
			w := do(h, http.MethodPost, "Note Hook", secret, noteHookTyped(tt.author, tt.note, tt.typ, tt.disc))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			if tt.wantKind == "" {
				if len(rec.jobs) != 0 {
					t.Fatalf("expected no job, got %+v", rec.jobs)
				}
				return
			}
			if len(rec.jobs) != 1 {
				t.Fatalf("expected 1 job, got %+v", rec.jobs)
			}
			j := rec.jobs[0]
			if j.Kind != tt.wantKind || j.ProjectID != 42 || j.ProjectPath != "group/sub/api" || j.MRIID != 7 ||
				j.UserID != 11 || j.Username != tt.author || j.NoteID != 555 || j.DiscussionID != tt.disc {
				t.Fatalf("job %+v", j)
			}
			if tt.wantKind == core.JobThreadReply && (j.Question != "" || !j.EnqueuedAt.Equal(fixedNow)) {
				t.Fatalf("job %+v", j)
			}
		})
	}
}

func TestThreadReplyIgnoresSystemAndCommitNotes(t *testing.T) {
	rec := &recorder{}
	h := newLearnHandler(rec, true)
	do(h, http.MethodPost, "Note Hook", secret, noteHook("bob", "changed the description", true, "MergeRequest"))
	do(h, http.MethodPost, "Note Hook", secret, noteHook("bob", "nice commit", false, "Commit"))
	if len(rec.jobs) != 0 {
		t.Fatalf("jobs %+v", rec.jobs)
	}
}

func TestThreadReplyOutsideGroups(t *testing.T) {
	rec := &recorder{}
	h := NewHandler(Options{Secret: secret, Enqueue: rec.enqueue, Router: events.NewRouter(events.Options{BotUsername: "reviewbot", LearnReplies: true, Groups: []string{"other"}})})
	do(h, http.MethodPost, "Note Hook", secret, noteHookTyped("bob", "так задумано", "DiscussionNote", "d1"))
	if len(rec.jobs) != 0 {
		t.Fatalf("jobs %+v", rec.jobs)
	}
}
