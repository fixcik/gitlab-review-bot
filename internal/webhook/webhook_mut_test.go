package webhook

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
)

func noteFor(projectID, topLevelProjectID, iid, noteID int64, body string, threaded bool) *notePayload {
	p := &notePayload{ProjectID: topLevelProjectID}
	p.Project = project{ID: projectID, PathWithNamespace: "team/api"}
	p.User = user{ID: 1, Username: "alice"}
	a := &p.ObjectAttributes
	a.ID, a.Note, a.NoteableType, a.DiscussionID = noteID, body, "MergeRequest", "d1"
	if threaded {
		a.Type = "DiscussionNote"
	}
	p.MergeRequest = &struct {
		IID int64 `json:"iid"`
	}{IID: iid}
	return p
}

// Note payloads that only carry the top-level project_id still route to that
// project; a payload without any project id, MR iid or (for thread replies)
// note id is dropped instead of producing a job that targets nothing.
func TestNoteProjectIDFallbackAndMissingIDs(t *testing.T) {
	h := NewHandler(Options{Router: events.NewRouter(events.Options{BotUsername: "reviewbot", LearnReplies: true})}).(*handler)

	for _, body := range []string{"@reviewbot review", "так задумано"} {
		job, why := h.noteJob(noteFor(0, 42, 3, 9, body, true))
		if job == nil || job.ProjectID != 42 {
			t.Fatalf("%q: top-level project_id not used: %+v (%s)", body, job, why)
		}
		job, _ = h.noteJob(noteFor(7, 42, 3, 9, body, true))
		if job == nil || job.ProjectID != 7 {
			t.Fatalf("%q: project.id must win: %+v", body, job)
		}
	}
	for name, p := range map[string]*notePayload{
		"command, no project":   noteFor(0, 0, 3, 9, "@reviewbot review", false),
		"command, no iid":       noteFor(7, 0, 0, 9, "@reviewbot review", false),
		"reply, no project":     noteFor(0, 0, 3, 9, "ok", true),
		"reply, no iid":         noteFor(7, 7, 0, 9, "ok", true),
		"reply, no note id":     noteFor(7, 7, 3, 0, "ok", true),
		"reply, only note id 0": noteFor(0, 7, 3, 0, "ok", true),
	} {
		if job, _ := h.noteJob(p); job != nil {
			t.Errorf("%s: want dropped, got %+v", name, job)
		}
	}
	// A command needs no note id.
	if job, _ := h.noteJob(noteFor(7, 7, 3, 0, "@reviewbot review", false)); job == nil || job.Kind != core.JobReview {
		t.Errorf("command without note id dropped: %+v", job)
	}
}

// Without a configured mention (no bot username), nothing is enqueued from
// notes, thread replies included.
func TestNoMentionConfiguredDropsNotes(t *testing.T) {
	h := NewHandler(Options{Router: events.NewRouter(events.Options{LearnReplies: true})}).(*handler)
	if job, why := h.noteJob(noteFor(7, 7, 3, 9, "так задумано", true)); job != nil {
		t.Fatalf("got %+v", job)
	} else if why != "no mention configured" {
		t.Fatalf("why = %q", why)
	}
}

func TestMRHookMissingIDsDropped(t *testing.T) {
	h := NewHandler(Options{Router: events.NewRouter(events.Options{BotUsername: "reviewbot"})}).(*handler)
	mk := func(pid, iid int64) *mrPayload {
		p := &mrPayload{}
		p.Project = project{ID: pid, PathWithNamespace: "team/api"}
		p.ObjectAttributes.IID, p.ObjectAttributes.State, p.ObjectAttributes.Action = iid, "opened", "open"
		return p
	}
	if job, _ := h.mrJob(mk(7, 3)); job == nil {
		t.Fatal("valid MR event dropped")
	}
	for _, p := range []*mrPayload{mk(0, 3), mk(7, 0)} {
		if job, _ := h.mrJob(p); job != nil {
			t.Errorf("want dropped, got %+v", job)
		}
	}
}

// A note claiming to be on a merge request but without the merge_request
// object, or a note on another noteable type, is ignored.
func TestNoteWithoutMergeRequestIgnored(t *testing.T) {
	h := NewHandler(Options{Router: events.NewRouter(events.Options{BotUsername: "reviewbot"})}).(*handler)
	p := noteFor(7, 7, 3, 9, "@reviewbot review", false)
	p.MergeRequest = nil
	if job, _ := h.noteJob(p); job != nil {
		t.Fatalf("got %+v", job)
	}
	p = noteFor(7, 7, 3, 9, "@reviewbot review", false)
	p.ObjectAttributes.NoteableType = "Issue"
	if job, _ := h.noteJob(p); job != nil {
		t.Fatalf("issue note: got %+v", job)
	}
}
