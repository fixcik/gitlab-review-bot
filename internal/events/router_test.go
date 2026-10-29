package events

import (
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestRouterMatchesWebhookRules(t *testing.T) {
	at := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	r := NewRouter(Options{BotUsername: "reviewbot", Groups: []string{"team"}, LearnReplies: true, Now: func() time.Time { return at }})
	base := MREvent{ProjectID: 7, ProjectPath: "team/api", IID: 3, State: "opened", HeadSHA: "abc"}

	cases := []struct {
		name string
		e    func(e MREvent) MREvent
		kind core.JobKind // "" = ignored
		full bool
	}{
		{"open", func(e MREvent) MREvent { e.Action = "open"; return e }, core.JobAutoReview, false},
		{"reopen", func(e MREvent) MREvent { e.Action = "reopen"; return e }, core.JobAutoReview, false},
		{"draft open", func(e MREvent) MREvent { e.Action = "open"; e.Draft = true; return e }, "", false},
		{"push", func(e MREvent) MREvent { e.Action = "update"; e.OldRev = "old"; return e }, core.JobAutoReview, false},
		{"ready", func(e MREvent) MREvent { e.Action = "update"; e.BecameReady = true; return e }, core.JobAutoReview, true},
		{"title", func(e MREvent) MREvent { e.Action = "update"; e.OldTitle, e.NewTitle = "a", "b"; return e }, core.JobRefreshTitle, false},
		{"nothing", func(e MREvent) MREvent { e.Action = "update"; return e }, "", false},
		{"closed", func(e MREvent) MREvent { e.Action = "update"; e.State = "closed"; e.OldRev = "x"; return e }, "", false},
		{"other group", func(e MREvent) MREvent { e.Action = "open"; e.ProjectPath = "x/api"; return e }, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, why := r.MR(tc.e(base))
			if tc.kind == "" {
				if job != nil {
					t.Fatalf("want ignored, got %+v", job)
				}
				return
			}
			if job == nil {
				t.Fatalf("ignored: %s", why)
			}
			if job.Kind != tc.kind || job.ForceFull != tc.full || !job.EnqueuedAt.Equal(at) {
				t.Errorf("job %+v", job)
			}
		})
	}

	n := NoteEvent{ProjectID: 7, ProjectPath: "team/api", IID: 3, NoteID: 9, DiscussionID: "d", UserID: 1, Username: "alice"}
	check := func(e NoteEvent, want core.JobKind) {
		t.Helper()
		job, why := r.Note(e)
		if want == "" {
			if job != nil {
				t.Fatalf("note %q: want ignored, got %+v", e.Body, job)
			}
			return
		}
		if job == nil || job.Kind != want || job.NoteID != 9 || job.DiscussionID != "d" || job.Username != "alice" {
			t.Fatalf("note %q: got %+v (%s), want %s", e.Body, job, why, want)
		}
	}
	e := n
	e.Body = "@reviewbot full review"
	check(e, core.JobFullReview)
	e.Body = "@reviewbot забудь #3"
	check(e, core.JobRules)
	e.Body = "так задумано"
	check(e, "") // plain comment
	e.Threaded = true
	check(e, core.JobThreadReply)
	e.System = true
	check(e, "")
	e = n
	e.Username, e.Body = "reviewbot", "@reviewbot review"
	check(e, "") // own note
}

func TestExcludedProjects(t *testing.T) {
	excluded := func(p string) bool { return p == "team/legacy" }
	r := NewRouter(Options{BotUsername: "reviewbot", Groups: []string{"team"}, Excluded: excluded})
	if r.Accepts("team/legacy") || !r.Accepts("team/api") || r.Accepts("other/x") {
		t.Error("Accepts")
	}
	job, why := r.MR(MREvent{ProjectID: 1, ProjectPath: "team/legacy", IID: 1, State: "opened", Action: "open", HeadSHA: "a"})
	if job != nil || !strings.Contains(why, "excluded") {
		t.Errorf("excluded MR event: %+v %s", job, why)
	}
	job, _ = r.Note(NoteEvent{ProjectID: 1, ProjectPath: "team/legacy", IID: 1, NoteID: 5, Body: "@reviewbot review", Username: "a"})
	if job != nil {
		t.Error("excluded note must be dropped")
	}
}
