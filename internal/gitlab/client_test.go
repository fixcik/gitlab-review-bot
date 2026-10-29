package gitlab

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

const testToken = "glpat-secret-token"

type recorded struct {
	Method  string
	RawPath string // escaped request path
	Query   string
	Body    string
	Token   string
	CType   string
}

type fakeGitLab struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	req []recorded
	h   http.HandlerFunc
}

func newFake(t *testing.T, h http.HandlerFunc) (*fakeGitLab, *Client) {
	t.Helper()
	f := &fakeGitLab{t: t, h: h}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.req = append(f.req, recorded{
			Method:  r.Method,
			RawPath: r.URL.EscapedPath(),
			Query:   r.URL.RawQuery,
			Body:    string(b),
			Token:   r.Header.Get("PRIVATE-TOKEN"),
			CType:   r.Header.Get("Content-Type"),
		})
		f.mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		f.h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	c, err := New(f.srv.URL, testToken, WithRetry(3, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func (f *fakeGitLab) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.req...)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestNew(t *testing.T) {
	tests := []struct {
		base    string
		token   string
		want    string
		wantErr bool
	}{
		{"https://gitlab.example.com", "t", "https://gitlab.example.com/api/v4", false},
		{"https://gitlab.example.com/", "t", "https://gitlab.example.com/api/v4", false},
		{"https://gitlab.example.com/api/v4/", "t", "https://gitlab.example.com/api/v4", false},
		{"https://example.com/gitlab", "t", "https://example.com/gitlab/api/v4", false},
		{"gitlab.example.com", "t", "", true},
		{"https://gitlab.example.com", "", "", true},
		{"ftp://x", "t", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.base, func(t *testing.T) {
			c, err := New(tt.base, tt.token)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && c.base.String() != tt.want {
				t.Fatalf("base = %s, want %s", c.base, tt.want)
			}
		})
	}
}

func TestCurrentUserAndAuthHeader(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"id": 7, "username": "reviewbot"})
	})
	id, name, err := c.CurrentUser(context.Background())
	if err != nil || id != 7 || name != "reviewbot" {
		t.Fatalf("got %d %q %v", id, name, err)
	}
	if got := f.requests()[0].Token; got != testToken {
		t.Fatalf("PRIVATE-TOKEN = %q", got)
	}
}

func TestGetMR(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v4/projects/42/merge_requests/7" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{
			"id": 1000, "iid": 7, "project_id": 42, "title": "Add X", "description": "desc",
			"state": "opened", "draft": true, "work_in_progress": true,
			"source_branch": "feature", "target_branch": "main",
			"author": {"id": 5, "username": "alice"},
			"web_url": "https://gitlab/x/-/merge_requests/7",
			"sha": "headsha",
			"diff_refs": {"base_sha": "b", "start_sha": "s", "head_sha": "h"}
		}`)
	})
	mr, err := c.GetMR(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := core.MergeRequest{
		ProjectID: 42, IID: 7, Title: "Add X", Description: "desc", State: "opened", Draft: true,
		SourceBranch: "feature", TargetBranch: "main", AuthorUsername: "alice",
		WebURL:          "https://gitlab/x/-/merge_requests/7",
		DiffRefs:        core.DiffRefs{BaseSHA: "b", StartSHA: "s", HeadSHA: "h"},
		DivergedCommits: -1,
	}
	if *mr != want {
		t.Fatalf("got %+v\nwant %+v", *mr, want)
	}
}

func TestGetMRStatusFields(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"iid": 7, "project_id": 42, "state": "opened", "created_at": "2026-10-20T10:00:00Z",
			"diverged_commits_count": 12,
			"head_pipeline": {"id": 9, "status": "failed", "sha": "h", "web_url": "https://gitlab/x/-/pipelines/9"},
			"diff_refs": {"base_sha": "b", "start_sha": "s", "head_sha": "h"}}`)
	})
	mr, err := c.GetMR(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if mr.DivergedCommits != 12 || mr.Pipeline == nil || *mr.Pipeline != (core.Pipeline{ID: 9, Status: "failed", SHA: "h", WebURL: "https://gitlab/x/-/pipelines/9"}) ||
		mr.CreatedAt.Format(time.RFC3339) != "2026-10-20T10:00:00Z" {
		t.Fatalf("got %+v pipeline %+v", mr, mr.Pipeline)
	}
	if q := f.requests()[0].Query; !strings.Contains(q, "include_diverged_commits_count=true") {
		t.Errorf("query %q", q)
	}
}

func TestGetMRDiffsPagination(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		var items []map[string]any
		switch page {
		case 1:
			w.Header().Set("X-Next-Page", "2")
			items = []map[string]any{{"old_path": "a", "new_path": "a", "diff": "@@ -1 +1 @@\n-x\n+y\n"}}
		case 2:
			w.Header().Set("X-Next-Page", "3")
			items = []map[string]any{{"old_path": "b", "new_path": "c", "renamed_file": true}}
		case 3:
			w.Header().Set("X-Next-Page", "")
			items = []map[string]any{
				{"old_path": "d", "new_path": "d", "new_file": true},
				{"old_path": "e", "new_path": "e", "deleted_file": true},
			}
		default:
			t.Errorf("unexpected page %d", page)
		}
		writeJSON(w, items)
	})
	diffs, err := c.GetMRDiffs(context.Background(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.FileDiff{
		{OldPath: "a", NewPath: "a", Diff: "@@ -1 +1 @@\n-x\n+y\n"},
		{OldPath: "b", NewPath: "c", RenamedFile: true},
		{OldPath: "d", NewPath: "d", NewFile: true},
		{OldPath: "e", NewPath: "e", DeletedFile: true},
	}
	if fmt.Sprint(diffs) != fmt.Sprint(want) {
		t.Fatalf("got %+v\nwant %+v", diffs, want)
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	if reqs[0].RawPath != "/api/v4/projects/1/merge_requests/2/diffs" {
		t.Fatalf("path = %s", reqs[0].RawPath)
	}
}

func TestListAndGetDiscussions(t *testing.T) {
	disc := `{"id": "abc123", "individual_note": false, "notes": [
		{"id": 1, "body": "<!-- reviewbot:finding id=x -->", "system": false,
		 "author": {"id": 9, "username": "bot"}, "created_at": "2026-01-02T03:04:05.000Z",
		 "resolvable": true, "resolved": true,
		 "position": {"base_sha": "b", "start_sha": "s", "head_sha": "h", "old_path": "o.go",
		              "new_path": "n.go", "old_line": null, "new_line": 12, "position_type": "text"}},
		{"id": 2, "body": "thanks", "system": false, "author": {"id": 5, "username": "alice"},
		 "created_at": "2026-01-02T04:00:00Z", "resolvable": true, "resolved": true}
	]}`
	sys := `{"id": "sys1", "individual_note": true, "notes": [
		{"id": 3, "body": "added 1 commit", "system": true, "author": {"id": 5, "username": "alice"},
		 "created_at": "2026-01-02T05:00:00Z", "resolvable": false, "resolved": false}]}`
	open := `{"id": "open1", "notes": [
		{"id": 4, "body": "q", "author": {"id": 5, "username": "alice"}, "created_at": "2026-01-02T05:00:00Z",
		 "resolvable": true, "resolved": false,
		 "position": {"base_sha": "b", "start_sha": "s", "head_sha": "h", "old_path": "a", "new_path": "a", "old_line": 3, "new_line": 4}}]}`

	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/1/merge_requests/2/discussions":
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Next-Page", "2")
				io.WriteString(w, "["+disc+","+sys+"]")
			} else {
				io.WriteString(w, "["+open+"]")
			}
		case "/api/v4/projects/1/merge_requests/2/discussions/abc123":
			io.WriteString(w, disc)
		default:
			http.NotFound(w, r)
		}
	})
	ds, err := c.ListDiscussions(context.Background(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 3 {
		t.Fatalf("len = %d", len(ds))
	}
	d := ds[0]
	if d.ID != "abc123" || !d.Resolvable || !d.Resolved || len(d.Notes) != 2 {
		t.Fatalf("discussion = %+v", d)
	}
	n := d.Notes[0]
	wantPos := core.Position{BaseSHA: "b", StartSHA: "s", HeadSHA: "h", OldPath: "o.go", NewPath: "n.go", NewLine: 12}
	if n.ID != 1 || n.AuthorID != 9 || n.AuthorUsername != "bot" || n.System || n.Position == nil || *n.Position != wantPos {
		t.Fatalf("note = %+v pos=%+v", n, n.Position)
	}
	if !n.CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("created_at = %v", n.CreatedAt)
	}
	if d.Notes[1].Position != nil {
		t.Fatal("note without position must have nil Position")
	}
	if s := ds[1]; s.Resolvable || s.Resolved || !s.Notes[0].System {
		t.Fatalf("system discussion = %+v", s)
	}
	if o := ds[2]; !o.Resolvable || o.Resolved || o.Notes[0].Position.OldLine != 3 || o.Notes[0].Position.NewLine != 4 {
		t.Fatalf("open discussion = %+v", o)
	}

	one, err := c.GetDiscussion(context.Background(), 1, 2, "abc123")
	if err != nil || one.ID != "abc123" || len(one.Notes) != 2 {
		t.Fatalf("GetDiscussion = %+v %v", one, err)
	}
}

func TestCreateDraftNotePosition(t *testing.T) {
	refs := core.DiffRefs{BaseSHA: "b", StartSHA: "s", HeadSHA: "h"}
	tests := []struct {
		name string
		pos  *core.Position
		want map[string]any
	}{
		{
			name: "added line: new_line only",
			pos:  &core.Position{BaseSHA: refs.BaseSHA, StartSHA: refs.StartSHA, HeadSHA: refs.HeadSHA, OldPath: "a.go", NewPath: "a.go", NewLine: 10},
			want: map[string]any{"note": "body", "position": map[string]any{
				"position_type": "text", "base_sha": "b", "start_sha": "s", "head_sha": "h",
				"old_path": "a.go", "new_path": "a.go", "new_line": float64(10)}},
		},
		{
			name: "context line: old_line and new_line",
			pos:  &core.Position{BaseSHA: "b", StartSHA: "s", HeadSHA: "h", OldPath: "old.go", NewPath: "new.go", OldLine: 8, NewLine: 10},
			want: map[string]any{"note": "body", "position": map[string]any{
				"position_type": "text", "base_sha": "b", "start_sha": "s", "head_sha": "h",
				"old_path": "old.go", "new_path": "new.go", "old_line": float64(8), "new_line": float64(10)}},
		},
		{
			name: "removed line: old_line only",
			pos:  &core.Position{BaseSHA: "b", StartSHA: "s", HeadSHA: "h", OldPath: "a.go", NewPath: "a.go", OldLine: 3},
			want: map[string]any{"note": "body", "position": map[string]any{
				"position_type": "text", "base_sha": "b", "start_sha": "s", "head_sha": "h",
				"old_path": "a.go", "new_path": "a.go", "old_line": float64(3)}},
		},
		{
			name: "no position",
			pos:  nil,
			want: map[string]any{"note": "body"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v4/projects/3/merge_requests/4/draft_notes" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(http.StatusCreated)
				writeJSON(w, map[string]any{"id": 55})
			})
			id, err := c.CreateDraftNote(context.Background(), 3, 4, "body", tt.pos)
			if err != nil || id != 55 {
				t.Fatalf("id=%d err=%v", id, err)
			}
			req := f.requests()[0]
			if req.CType != "application/json" {
				t.Fatalf("content-type = %q", req.CType)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(req.Body), &got); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("body = %v\nwant  %v", got, tt.want)
			}
		})
	}
}

func TestDraftNotesPublishAndDelete(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && p == "/api/v4/projects/1/merge_requests/2/draft_notes/bulk_publish":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && p == "/api/v4/projects/1/merge_requests/2/draft_notes":
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Next-Page", "2")
				writeJSON(w, []map[string]any{{"id": 11}, {"id": 12}})
			} else {
				writeJSON(w, []map[string]any{{"id": 13}})
			}
		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/api/v4/projects/1/merge_requests/2/draft_notes/"):
			if strings.HasSuffix(p, "/12") {
				http.NotFound(w, r) // already gone: ignored
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	if err := c.PublishDraftNotes(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDraftNotes(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	for _, r := range f.requests() {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.RawPath[strings.LastIndex(r.RawPath, "/")+1:])
		}
	}
	if strings.Join(deleted, ",") != "11,12,13" {
		t.Fatalf("deleted = %v", deleted)
	}
}

func TestNotesAndDiscussionWrites(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.EscapedPath() {
		case "POST /api/v4/projects/1/merge_requests/2/notes":
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": 100})
		case "PUT /api/v4/projects/1/merge_requests/2/notes/100",
			"POST /api/v4/projects/1/merge_requests/2/discussions/d1/notes",
			"PUT /api/v4/projects/1/merge_requests/2/discussions/d1":
			writeJSON(w, map[string]any{"id": 1})
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	id, err := c.CreateNote(ctx, 1, 2, "summary")
	if err != nil || id != 100 {
		t.Fatalf("CreateNote = %d %v", id, err)
	}
	if err := c.UpdateNote(ctx, 1, 2, 100, "summary v2"); err != nil {
		t.Fatal(err)
	}
	if err := c.ReplyToDiscussion(ctx, 1, 2, "d1", "reply"); err != nil {
		t.Fatal(err)
	}
	if err := c.ResolveDiscussion(ctx, 1, 2, "d1", true); err != nil {
		t.Fatal(err)
	}
	if err := c.ResolveDiscussion(ctx, 1, 2, "d1", false); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	wantBodies := []string{
		`{"body":"summary"}`,
		`{"body":"summary v2"}`,
		`{"body":"reply"}`,
		`{"resolved":true}`,
		`{"resolved":false}`,
	}
	if len(reqs) != len(wantBodies) {
		t.Fatalf("requests = %d", len(reqs))
	}
	for i, want := range wantBodies {
		if reqs[i].Body != want {
			t.Errorf("request %d body = %s, want %s", i, reqs[i].Body, want)
		}
	}
}

func TestAccessLevel(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    int
		wantErr bool
	}{
		{"developer", 200, `{"id": 5, "access_level": 30}`, 30, false},
		{"maintainer", 200, `{"id": 5, "access_level": 40}`, 40, false},
		{"not a member", 404, `{"message":"404 Not found"}`, 0, false},
		{"forbidden", 403, `{"message":"403 Forbidden"}`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			})
			got, err := c.AccessLevel(context.Background(), 8, 5)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("got %d %v, want %d wantErr=%v", got, err, tt.want, tt.wantErr)
			}
			if p := f.requests()[0].RawPath; p != "/api/v4/projects/8/members/all/5" {
				t.Fatalf("path = %s", p)
			}
		})
	}
}

func TestGetRawFileEscaping(t *testing.T) {
	tests := []struct {
		name      string
		path, ref string
		wantPath  string
		wantQuery string
	}{
		{"nested with spaces", "dir/sub dir/my file.yaml", "main",
			"/api/v4/projects/9/repository/files/dir%2Fsub%20dir%2Fmy%20file.yaml/raw", "ref=main"},
		{"dotfile", ".reviewbot.yaml", "feature/x",
			"/api/v4/projects/9/repository/files/.reviewbot.yaml/raw", "ref=feature%2Fx"},
		{"special chars", "a/b+c#d?.txt", "v1.0",
			"/api/v4/projects/9/repository/files/a%2Fb+c%23d%3F.txt/raw", "ref=v1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "content")
			})
			b, err := c.GetRawFile(context.Background(), 9, tt.path, tt.ref)
			if err != nil || string(b) != "content" {
				t.Fatalf("got %q %v", b, err)
			}
			req := f.requests()[0]
			if req.RawPath != tt.wantPath {
				t.Fatalf("path  = %s\nwant    %s", req.RawPath, tt.wantPath)
			}
			if req.Query != tt.wantQuery {
				t.Fatalf("query = %s, want %s", req.Query, tt.wantQuery)
			}
		})
	}
}

func TestNotFoundMapping(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"404 File Not Found"}`)
	})
	ctx := context.Background()
	calls := map[string]func() error{
		"GetRawFile":    func() error { _, err := c.GetRawFile(ctx, 1, "x", "main"); return err },
		"GetMR":         func() error { _, err := c.GetMR(ctx, 1, 2); return err },
		"GetDiscussion": func() error { _, err := c.GetDiscussion(ctx, 1, 2, "d"); return err },
		"UpdateNote":    func() error { return c.UpdateNote(ctx, 1, 2, 3, "b") },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			if StatusCode(err) != 404 {
				t.Fatalf("status = %d", StatusCode(err))
			}
		})
	}
}

func TestErrorBodyTruncatedAndNoToken(t *testing.T) {
	long := strings.Repeat("x", 5000)
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"message":"bad `+r.Header.Get("PRIVATE-TOKEN")+`"}`+long)
	})
	_, err := c.GetMR(context.Background(), 1, 2)
	var ge *Error
	if !errors.As(err, &ge) || ge.StatusCode != 400 {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("error leaks token")
	}
	if len(ge.Body) > maxErrorBody+50 {
		t.Fatalf("body not truncated: %d", len(ge.Body))
	}
	if errors.Is(err, core.ErrNotFound) {
		t.Fatal("400 must not be ErrNotFound")
	}
}

func TestRetry(t *testing.T) {
	ok := func(w http.ResponseWriter) { writeJSON(w, map[string]any{"id": 1, "username": "u"}) }
	tests := []struct {
		name      string
		call      func(c *Client) error
		responses []int // status per attempt; 200 = success
		wantCalls int
		wantErr   bool
	}{
		{"GET 429 then ok", func(c *Client) error { _, _, err := c.CurrentUser(context.Background()); return err },
			[]int{429, 200}, 2, false},
		{"GET 502,503 then ok", func(c *Client) error { _, _, err := c.CurrentUser(context.Background()); return err },
			[]int{502, 503, 200}, 3, false},
		{"GET gives up after 3 retries", func(c *Client) error { _, _, err := c.CurrentUser(context.Background()); return err },
			[]int{500, 500, 500, 500, 200}, 4, true},
		{"GET 400 not retried", func(c *Client) error { _, _, err := c.CurrentUser(context.Background()); return err },
			[]int{400, 200}, 1, true},
		{"PUT 500 retried", func(c *Client) error { return c.UpdateNote(context.Background(), 1, 2, 3, "b") },
			[]int{500, 200}, 2, false},
		{"POST note 500 not retried", func(c *Client) error { _, err := c.CreateNote(context.Background(), 1, 2, "b"); return err },
			[]int{500, 200}, 1, true},
		{"POST draft 500 not retried", func(c *Client) error {
			_, err := c.CreateDraftNote(context.Background(), 1, 2, "b", nil)
			return err
		}, []int{500, 200}, 1, true},
		{"POST reply 503 not retried", func(c *Client) error { return c.ReplyToDiscussion(context.Background(), 1, 2, "d", "b") },
			[]int{503, 200}, 1, true},
		{"POST note 429 retried", func(c *Client) error { _, err := c.CreateNote(context.Background(), 1, 2, "b"); return err },
			[]int{429, 200}, 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var n atomic.Int32
			_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				i := int(n.Add(1)) - 1
				st := 200
				if i < len(tt.responses) {
					st = tt.responses[i]
				}
				if st != 200 {
					w.WriteHeader(st)
					io.WriteString(w, `{"message":"err"}`)
					return
				}
				ok(w)
			})
			err := tt.call(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := int(n.Load()); got != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestRetryAfterHonored(t *testing.T) {
	var times []time.Time
	var mu sync.Mutex
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		first := len(times) == 1
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(w, map[string]any{"id": 1, "username": "u"})
	})
	if _, _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(times) != 2 {
		t.Fatalf("calls = %d", len(times))
	}
	if d := times[1].Sub(times[0]); d < 900*time.Millisecond {
		t.Fatalf("retried after %v, want >= ~1s (Retry-After)", d)
	}
}

func TestRetryRespectsContext(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := c.CurrentUser(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("did not stop on context cancellation")
	}
}

func TestTransportErrorRetry(t *testing.T) {
	// Closed server: connection refused. GET retried, POST not.
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	var calls atomic.Int32
	hc := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return http.DefaultTransport.RoundTrip(r)
	})}
	c, err := New(base, testToken, WithHTTPClient(hc), WithRetry(2, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.CurrentUser(context.Background()); err == nil {
		t.Fatal("expected error")
	} else if strings.Contains(err.Error(), testToken) {
		t.Fatal("token leaked")
	}
	if calls.Load() != 3 {
		t.Fatalf("GET attempts = %d, want 3", calls.Load())
	}
	calls.Store(0)
	if _, err := c.CreateNote(context.Background(), 1, 2, "x"); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("POST attempts = %d, want 1", calls.Load())
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"5", 5 * time.Second},
		{"-1", 0},
		{"junk", 0},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > 11*time.Second {
		t.Errorf("http-date Retry-After = %v", got)
	}
}

func TestWithCAFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": 1, "username": "tls"})
	}))
	defer srv.Close()

	// Without the CA the request fails verification.
	c0, err := New(srv.URL, testToken, WithRetry(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c0.CurrentUser(context.Background()); err == nil {
		t.Fatal("expected TLS verification failure")
	}

	pemPath := filepath.Join(t.TempDir(), "ca.pem")
	cert := srv.Certificate()
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(pemPath, pemData, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(srv.URL, testToken, WithCAFile(pemPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, name, err := c.CurrentUser(context.Background()); err != nil || name != "tls" {
		t.Fatalf("got %q %v", name, err)
	}

	if _, err := New(srv.URL, testToken, WithCAFile(filepath.Join(t.TempDir(), "missing.pem"))); err == nil {
		t.Fatal("missing CA file must fail")
	}
}

func TestShortTokenNotRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"message":"see https://g.example for details"}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "x", WithRetry(0, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetMR(context.Background(), 1, 2)
	if err == nil || !strings.Contains(err.Error(), "https://g.example") {
		t.Fatalf("error text corrupted: %v", err)
	}
	if strings.Contains(err.Error(), "REDACTED") {
		t.Fatalf("short token must not be redacted: %v", err)
	}
	if got := scrub(errors.New("dial https://g.example"), "x").Error(); got != "dial https://g.example" {
		t.Fatalf("scrub corrupted message: %q", got)
	}
	if got := scrub(errors.New("bad "+testToken), testToken).Error(); strings.Contains(got, testToken) {
		t.Fatalf("scrub leaked token: %q", got)
	}
}

func TestGetRawFileMaxCapsTheRead(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 100))
	})
	if _, err := c.GetRawFileMax(context.Background(), 9, ".reviewbot.yaml", "main", 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the cap: %v", err)
	}
	if b, err := c.GetRawFileMax(context.Background(), 9, ".reviewbot.yaml", "main", 100); err != nil || len(b) != 100 {
		t.Fatalf("at the cap: %d %v", len(b), err)
	}
}
