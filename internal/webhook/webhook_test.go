package webhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
)

const secret = "s3cret"

var fixedNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

type recorder struct {
	mu   sync.Mutex
	jobs []core.Job
}

func (r *recorder) enqueue(j core.Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs = append(r.jobs, j)
}

func newTestHandler(rec *recorder, mention string) http.Handler {
	return NewHandler(Options{
		Secret:  secret,
		Enqueue: rec.enqueue,
		Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", Mention: mention, Now: func() time.Time { return fixedNow }}),
	})
}

func do(h http.Handler, method, event, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/webhook", strings.NewReader(body))
	if event != "" {
		req.Header.Set("X-Gitlab-Event", event)
	}
	if token != "" {
		req.Header.Set("X-Gitlab-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// mrHook builds a realistic Merge Request Hook payload.
func mrHook(action, state string, draft bool, extraAttrs, changes string) string {
	d := "false"
	if draft {
		d = "true"
	}
	if changes == "" {
		changes = "{}"
	}
	return `{
  "object_kind": "merge_request",
  "event_type": "merge_request",
  "user": {"id": 5, "name": "Alice", "username": "alice", "email": "[REDACTED]"},
  "project": {
    "id": 42, "name": "api", "web_url": "https://gitlab.example.com/group/sub/api",
    "path_with_namespace": "group/sub/api", "default_branch": "main"
  },
  "object_attributes": {
    "id": 9001, "iid": 7, "title": "Add feature", "description": "Some text",
    "source_branch": "feature", "target_branch": "main",
    "state": "` + state + `", "action": "` + action + `",
    "draft": ` + d + `, "work_in_progress": ` + d + `,
    "merge_status": "can_be_merged",
    ` + extraAttrs + `
    "last_commit": {"id": "abc123def4567890", "message": "wip", "timestamp": "2026-10-24T10:00:00+00:00"},
    "url": "https://gitlab.example.com/group/sub/api/-/merge_requests/7"
  },
  "labels": [],
  "changes": ` + changes + `,
  "repository": {"name": "api", "url": "git@gitlab.example.com:group/sub/api.git"}
}`
}

// noteHook builds a realistic Note Hook payload on an MR.
func noteHook(author, note string, system bool, noteable string) string {
	sys := "false"
	if system {
		sys = "true"
	}
	mr := `"merge_request": {"id": 9001, "iid": 7, "title": "Add feature", "state": "opened", "draft": true,
    "source_branch": "feature", "target_branch": "main", "last_commit": {"id": "abc123"}},`
	if noteable != "MergeRequest" {
		mr = `"commit": {"id": "abc123", "message": "x"},`
	}
	return `{
  "object_kind": "note",
  "event_type": "note",
  "user": {"id": 11, "name": "Bob", "username": "` + author + `", "email": "[REDACTED]"},
  "project_id": 42,
  "project": {"id": 42, "name": "api", "path_with_namespace": "group/sub/api"},
  "object_attributes": {
    "id": 555, "note": ` + jsonString(note) + `, "noteable_type": "` + noteable + `",
    "author_id": 11, "created_at": "2026-10-24 10:00:00 UTC", "project_id": 42,
    "system": ` + sys + `, "noteable_id": 9001, "discussion_id": "d1scuss10n",
    "type": "DiscussionNote",
    "url": "https://gitlab.example.com/group/sub/api/-/merge_requests/7#note_555"
  },
  ` + mr + `
  "repository": {"name": "api"}
}`
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

func TestAuthAndMethod(t *testing.T) {
	rec := &recorder{}
	h := newTestHandler(rec, "")
	body := mrHook("open", "opened", false, "", "")

	if w := do(h, http.MethodGet, "Merge Request Hook", secret, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d", w.Code)
	}
	if w := do(h, http.MethodPost, "Merge Request Hook", "", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d", w.Code)
	}
	if w := do(h, http.MethodPost, "Merge Request Hook", "wrong", body); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d", w.Code)
	}
	if w := do(h, http.MethodPost, "Merge Request Hook", secret, "{not json"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad json: got %d", w.Code)
	}
	if w := do(h, http.MethodPost, "Note Hook", secret, "[1,2"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad json note: got %d", w.Code)
	}
	if w := do(h, http.MethodPost, "Push Hook", secret, `{"object_kind":"push"}`); w.Code != http.StatusOK {
		t.Fatalf("other event: got %d", w.Code)
	}
	if len(rec.jobs) != 0 {
		t.Fatalf("unexpected jobs: %+v", rec.jobs)
	}
	big := `{"x":"` + strings.Repeat("a", MaxBodyBytes+10) + `"}`
	if w := do(h, http.MethodPost, "Note Hook", secret, big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body: got %d", w.Code)
	}
}

func TestEmptySecretRejectsEverything(t *testing.T) {
	rec := &recorder{}
	h := NewHandler(Options{Enqueue: rec.enqueue, Router: events.NewRouter(events.Options{BotUsername: "reviewbot"})})
	if w := do(h, http.MethodPost, "Merge Request Hook", "", mrHook("open", "opened", false, "", "")); w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", w.Code)
	}
}

func TestMergeRequestHook(t *testing.T) {
	draftReady := `{"draft": {"previous": true, "current": false}, "title": {"previous": "Draft: Add feature", "current": "Add feature"}}`
	draftOn := `{"draft": {"previous": false, "current": true}}`
	wipReady := `{"work_in_progress": {"previous": true, "current": false}}`
	tests := []struct {
		name      string
		body      string
		want      bool
		forceFull bool
	}{
		{"open", mrHook("open", "opened", false, "", ""), true, false},
		{"reopen", mrHook("reopen", "opened", false, "", ""), true, false},
		{"update with new commits", mrHook("update", "opened", false, `"oldrev": "0000aaaa1111",`, ""), true, false},
		{"draft to ready", mrHook("update", "opened", false, "", draftReady), true, true},
		{"draft to ready with commits", mrHook("update", "opened", false, `"oldrev": "0000aaaa1111",`, draftReady), true, true},
		{"wip to ready (legacy)", mrHook("update", "opened", false, "", wipReady), true, true},
		{"ready to draft", mrHook("update", "opened", true, "", draftOn), false, false},
		{"open draft", mrHook("open", "opened", true, "", ""), false, false},
		{"push to draft", mrHook("update", "opened", true, `"oldrev": "0000aaaa1111",`, ""), false, false},
		{"merged", mrHook("merge", "merged", false, "", ""), false, false},
		{"closed", mrHook("close", "closed", false, "", ""), false, false},
		{"approved", mrHook("approved", "opened", false, "", ""), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			h := newTestHandler(rec, "")
			w := do(h, http.MethodPost, "Merge Request Hook", secret, tt.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if !tt.want {
				if len(rec.jobs) != 0 {
					t.Fatalf("expected no job, got %+v", rec.jobs)
				}
				return
			}
			if len(rec.jobs) != 1 {
				t.Fatalf("expected 1 job, got %d", len(rec.jobs))
			}
			j := rec.jobs[0]
			want := core.Job{
				Kind: core.JobAutoReview, ProjectID: 42, ProjectPath: "group/sub/api", MRIID: 7,
				HeadSHA: "abc123def4567890", ForceFull: tt.forceFull, EnqueuedAt: fixedNow,
			}
			if j != want {
				t.Fatalf("job mismatch:\n got %+v\nwant %+v", j, want)
			}
		})
	}
}

func TestNoteHook(t *testing.T) {
	tests := []struct {
		name     string
		mention  string
		author   string
		note     string
		system   bool
		noteable string
		wantKind core.JobKind // "" = no job
		wantQ    string
	}{
		{"review", "", "bob", "@reviewbot review", false, "MergeRequest", core.JobReview, ""},
		{"review uppercase mention", "", "bob", "@ReviewBot Review", false, "MergeRequest", core.JobReview, ""},
		{"review ru", "", "bob", "@reviewbot ревью", false, "MergeRequest", core.JobReview, ""},
		{"review with punctuation", "", "bob", "@reviewbot, review!", false, "MergeRequest", core.JobReview, ""},
		{"full review", "", "bob", "@reviewbot full review", false, "MergeRequest", core.JobFullReview, ""},
		{"full-review", "", "bob", "@reviewbot full-review", false, "MergeRequest", core.JobFullReview, ""},
		{"full review ru", "", "bob", "@reviewbot Полное ревью", false, "MergeRequest", core.JobFullReview, ""},
		{"full review extra spaces", "", "bob", "@reviewbot   full    review  ", false, "MergeRequest", core.JobFullReview, ""},
		{"review force", "", "bob", "@reviewbot review force", false, "MergeRequest", core.JobForceReview, ""},
		{"force review", "", "bob", "@reviewbot Force review!", false, "MergeRequest", core.JobForceReview, ""},
		{"review force ru", "", "bob", "@reviewbot ревью принудительно", false, "MergeRequest", core.JobForceReview, ""},
		{"review force ru 2", "", "bob", "@reviewbot принудительное ревью.", false, "MergeRequest", core.JobForceReview, ""},
		{"review force multiline is chat", "", "bob", "@reviewbot review force\nи ещё вопрос", false, "MergeRequest", core.JobChat, "review force\nи ещё вопрос"},
		{"help", "", "bob", "@reviewbot help", false, "MergeRequest", core.JobHelp, ""},
		{"help ru", "", "bob", "@reviewbot помощь", false, "MergeRequest", core.JobHelp, ""},
		{"bare mention", "", "bob", "@reviewbot", false, "MergeRequest", core.JobHelp, ""},
		{"chat", "", "bob", "@reviewbot why is this lock needed?", false, "MergeRequest", core.JobChat, "why is this lock needed?"},
		{"chat starting with review word", "", "bob", "@reviewbot review the error handling please", false, "MergeRequest", core.JobChat, "review the error handling please"},
		{"chat multiline", "", "bob", "@reviewbot review\nи объясни функцию Foo", false, "MergeRequest", core.JobChat, "review\nи объясни функцию Foo"},
		{"chat mention in middle", "", "bob", "Hey @reviewbot is this safe?", false, "MergeRequest", core.JobChat, "is this safe?"},
		{"chat mention at end", "", "bob", "Это точно потокобезопасно? @reviewbot", false, "MergeRequest", core.JobChat, "Это точно потокобезопасно?"},
		{"custom mention", "@ai", "bob", "@AI help", false, "MergeRequest", core.JobHelp, ""},
		{"custom mention ignores username", "@ai", "bob", "@reviewbot help", false, "MergeRequest", "", ""},
		{"longer username", "", "bob", "@reviewbot2 review", false, "MergeRequest", "", ""},
		{"username with dot suffix", "", "bob", "@reviewbot.old review", false, "MergeRequest", "", ""},
		{"mention end of sentence", "", "bob", "thanks @reviewbot.", false, "MergeRequest", core.JobChat, "thanks"},
		{"email-like", "", "bob", "mail me at x@reviewbot review", false, "MergeRequest", "", ""},
		{"no mention", "", "bob", "LGTM", false, "MergeRequest", "", ""},
		{"own note", "", "ReviewBot", "@reviewbot review", false, "MergeRequest", "", ""},
		{"system note", "", "bob", "@reviewbot review", true, "MergeRequest", "", ""},
		{"commit note", "", "bob", "@reviewbot review", false, "Commit", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			h := newTestHandler(rec, tt.mention)
			w := do(h, http.MethodPost, "Note Hook", secret, noteHook(tt.author, tt.note, tt.system, tt.noteable))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tt.wantKind == "" {
				if len(rec.jobs) != 0 {
					t.Fatalf("expected no job, got %+v", rec.jobs)
				}
				return
			}
			if len(rec.jobs) != 1 {
				t.Fatalf("expected 1 job, got %d", len(rec.jobs))
			}
			want := core.Job{
				Kind: tt.wantKind, ProjectID: 42, ProjectPath: "group/sub/api", MRIID: 7,
				UserID: 11, Username: tt.author, DiscussionID: "d1scuss10n", NoteID: 555,
				Question: tt.wantQ, EnqueuedAt: fixedNow,
			}
			if got := rec.jobs[0]; got != want {
				t.Fatalf("job mismatch:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestGroupsFilter(t *testing.T) {
	mr := mrHook("open", "opened", false, "", "")
	note := noteHook("bob", "@reviewbot review", false, "MergeRequest")
	withPath := func(body, path string) string {
		return strings.ReplaceAll(body, `"path_with_namespace": "group/sub/api"`, `"path_with_namespace": `+jsonString(path))
	}
	tests := []struct {
		name   string
		groups []string
		path   string
		want   bool
	}{
		{"no groups accepts all", nil, "other/api", true},
		{"exact group", []string{"group"}, "group/sub/api", true},
		{"subgroup", []string{"group/sub"}, "group/sub/api", true},
		{"case-insensitive", []string{"GROUP/Sub"}, "Group/SUB/api", true},
		{"group written with slashes and spaces", []string{" /group/ "}, "group/sub/api", true},
		{"one of several", []string{"backend", "group"}, "group/sub/api", true},
		{"project path equals group", []string{"group/sub/api"}, "group/sub/api", true},
		{"prefix without slash rejected", []string{"group"}, "groupie/api", false},
		{"foreign group rejected", []string{"backend"}, "group/sub/api", false},
		{"parent of configured subgroup rejected", []string{"group/sub"}, "group/api", false},
		{"empty path rejected when groups set", []string{"group"}, "", false},
		{"blank group entries ignored", []string{"", " / "}, "anything/api", true},
	}
	for _, tt := range tests {
		for _, ev := range []struct{ event, body string }{
			{"Merge Request Hook", mr},
			{"Note Hook", note},
		} {
			t.Run(tt.name+"/"+ev.event, func(t *testing.T) {
				rec := &recorder{}
				h := NewHandler(Options{
					Secret:  secret,
					Enqueue: rec.enqueue,
					Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", Groups: tt.groups}),
				})
				w := do(h, http.MethodPost, ev.event, secret, withPath(ev.body, tt.path))
				if w.Code != http.StatusOK {
					t.Fatalf("status %d", w.Code)
				}
				if got := len(rec.jobs) == 1; got != tt.want {
					t.Fatalf("accepted=%v, want %v (body %q)", got, tt.want, w.Body.String())
				}
				if !tt.want && !strings.Contains(w.Body.String(), "outside the configured groups") {
					t.Errorf("unexpected reason: %q", w.Body.String())
				}
			})
		}
	}
}

func TestMergeRequestHookTitleOnly(t *testing.T) {
	title := `{"title": {"previous": "a", "current": "b"}}`
	tests := []struct {
		name string
		body string
		want core.JobKind // "" = no job
	}{
		{"title only", mrHook("update", "opened", false, "", title), core.JobRefreshTitle},
		{"title unchanged", mrHook("update", "opened", false, "", `{"title": {"previous": "a", "current": "a"}}`), ""},
		{"title and commits", mrHook("update", "opened", false, `"oldrev": "0000aaaa1111",`, title), core.JobAutoReview},
		{"title on draft", mrHook("update", "opened", true, "", `{"title": {"previous": "Draft: a", "current": "Draft: b"}}`), ""},
		{"title on closed", mrHook("update", "closed", false, "", title), ""},
		{"description only", mrHook("update", "opened", false, "", `{"description": {"previous": "a", "current": "b"}}`), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			h := newTestHandler(rec, "")
			if w := do(h, http.MethodPost, "Merge Request Hook", secret, tt.body); w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			if tt.want == "" {
				if len(rec.jobs) != 0 {
					t.Fatalf("expected no job, got %+v", rec.jobs)
				}
				return
			}
			if len(rec.jobs) != 1 {
				t.Fatalf("expected 1 job, got %d", len(rec.jobs))
			}
			want := core.Job{Kind: tt.want, ProjectID: 42, ProjectPath: "group/sub/api", MRIID: 7, EnqueuedAt: fixedNow}
			if tt.want == core.JobAutoReview {
				want.HeadSHA = "abc123def4567890"
			}
			if got := rec.jobs[0]; got != want {
				t.Fatalf("job mismatch:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}
