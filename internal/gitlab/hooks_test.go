package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

const hookSecret = "hook-secret-value"

func TestListGroupProjectsPaginationAndEscaping(t *testing.T) {
	f, c := newHooksFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			writeJSON(w, []map[string]any{
				{"id": 1, "path_with_namespace": "platform/services/a", "archived": false},
				{"id": 2, "path_with_namespace": "platform/services/sub/b", "archived": true},
			})
		case "2":
			w.Header().Set("X-Next-Page", "")
			writeJSON(w, []map[string]any{{"id": 3, "path_with_namespace": "platform/services/c"}})
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	})
	got, err := c.ListGroupProjects(context.Background(), " /platform/services/ ")
	if err != nil {
		t.Fatal(err)
	}
	want := []Project{
		{ID: 1, PathWithNamespace: "platform/services/a"},
		{ID: 2, PathWithNamespace: "platform/services/sub/b", Archived: true},
		{ID: 3, PathWithNamespace: "platform/services/c"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %+v want %+v", i, got[i], want[i])
		}
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests: %d", len(reqs))
	}
	for _, r := range reqs {
		if r.Method != http.MethodGet || r.RawPath != "/api/v4/groups/platform%2Fservices/projects" {
			t.Errorf("request %s %s", r.Method, r.RawPath)
		}
		q, _ := url.ParseQuery(r.Query)
		if q.Get("include_subgroups") != "true" || q.Get("archived") != "false" || q.Get("with_shared") != "false" || q.Get("per_page") != "100" {
			t.Errorf("query %q", r.Query)
		}
		if r.Token != testToken {
			t.Errorf("token header %q", r.Token)
		}
	}
}

func TestListGroupProjectsEmptyPathAndNotFound(t *testing.T) {
	_, c := newHooksFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"404 Group Not Found"}`, http.StatusNotFound)
	})
	if _, err := c.ListGroupProjects(context.Background(), " / "); err == nil {
		t.Fatal("expected error for empty path")
	}
	_, err := c.ListGroupProjects(context.Background(), "missing")
	if !errors.Is(err, core.ErrNotFound) || StatusCode(err) != http.StatusNotFound {
		t.Fatalf("got %v", err)
	}
}

func TestListProjectHooks(t *testing.T) {
	f, c := newHooksFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			writeJSON(w, []map[string]any{{
				"id": 10, "url": "https://bot/webhook", "merge_requests_events": true,
				"note_events": true, "push_events": false, "enable_ssl_verification": true,
			}})
			return
		}
		writeJSON(w, []map[string]any{{"id": 11, "url": "https://ci/hook", "push_events": true}})
	})
	got, err := c.ListProjectHooks(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	want := []Hook{
		{ID: 10, URL: "https://bot/webhook", MergeRequestsEvents: true, NoteEvents: true, EnableSSLVerification: true},
		{ID: 11, URL: "https://ci/hook", PushEvents: true},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
	}
	if p := f.requests()[0].RawPath; p != "/api/v4/projects/42/hooks" {
		t.Errorf("path %q", p)
	}
}

func decodeBody(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("body %q: %v", s, err)
	}
	return m
}

func checkHookPayload(t *testing.T, body string, sslVerify bool) {
	t.Helper()
	m := decodeBody(t, body)
	want := map[string]any{
		"url":                     "https://bot/webhook",
		"token":                   hookSecret,
		"merge_requests_events":   true,
		"note_events":             true,
		"push_events":             false,
		"enable_ssl_verification": sslVerify,
	}
	for k, v := range want {
		got, ok := m[k]
		if !ok {
			t.Errorf("payload lacks %q (push_events must be explicit): %s", k, body)
			continue
		}
		if got != v {
			t.Errorf("payload %s = %v, want %v", k, got, v)
		}
	}
}

func TestAddProjectHook(t *testing.T) {
	f, c := newHooksFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"id": 77, "url": "https://bot/webhook"})
	})
	spec := HookSpec{URL: "https://bot/webhook", Token: hookSecret, MergeRequestsEvents: true, NoteEvents: true, EnableSSLVerification: false}
	hid, err := c.AddProjectHook(context.Background(), 42, spec)
	if err != nil {
		t.Fatal(err)
	}
	if hid != 77 {
		t.Errorf("id %d", hid)
	}
	r := f.requests()[0]
	if r.Method != http.MethodPost || r.RawPath != "/api/v4/projects/42/hooks" || r.CType != "application/json" {
		t.Errorf("request %s %s %s", r.Method, r.RawPath, r.CType)
	}
	checkHookPayload(t, r.Body, false)
}

func TestAddProjectHookNoRetryOn5xx(t *testing.T) {
	f, c := newHooksFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	_, err := c.AddProjectHook(context.Background(), 1, HookSpec{URL: "https://bot/webhook", Token: hookSecret})
	if err == nil {
		t.Fatal("expected error")
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("POST retried: %d requests", n)
	}
}

func TestEditProjectHook(t *testing.T) {
	f, c := newHooksFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": 9})
	})
	spec := HookSpec{URL: "https://bot/webhook", Token: hookSecret, MergeRequestsEvents: true, NoteEvents: true, EnableSSLVerification: true}
	if err := c.EditProjectHook(context.Background(), 42, 9, spec); err != nil {
		t.Fatal(err)
	}
	r := f.requests()[0]
	if r.Method != http.MethodPut || r.RawPath != "/api/v4/projects/42/hooks/9" {
		t.Errorf("request %s %s", r.Method, r.RawPath)
	}
	checkHookPayload(t, r.Body, true)
}

func TestHookErrorsNeverContainSecret(t *testing.T) {
	// GitLab echoing the request body in a validation error.
	_, c := newHooksFake(t, func(w http.ResponseWriter, r *http.Request) {
		b := strings.Builder{}
		b.WriteString(`{"message":"invalid: `)
		b.WriteString(hookSecret)
		b.WriteString(`"}`)
		http.Error(w, b.String(), http.StatusUnprocessableEntity)
	})
	spec := HookSpec{URL: "https://bot/webhook", Token: hookSecret, MergeRequestsEvents: true}
	_, err := c.AddProjectHook(context.Background(), 1, spec)
	if err == nil || strings.Contains(err.Error(), hookSecret) {
		t.Fatalf("add error leaks secret: %v", err)
	}
	if StatusCode(err) != http.StatusUnprocessableEntity {
		t.Errorf("status lost: %v", err)
	}
	err = c.EditProjectHook(context.Background(), 1, 2, spec)
	if err == nil || strings.Contains(err.Error(), hookSecret) {
		t.Fatalf("edit error leaks secret: %v", err)
	}
	if StatusCode(err) != http.StatusUnprocessableEntity {
		t.Errorf("status lost: %v", err)
	}
}

func TestHookForbidden(t *testing.T) {
	_, c := newHooksFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
	})
	_, err := c.ListProjectHooks(context.Background(), 5)
	if StatusCode(err) != http.StatusForbidden {
		t.Fatalf("got %v", err)
	}
}

// newHooksFake is newFake with the hooks-only write policy used by the
// hook-sync client.
func newHooksFake(t *testing.T, h http.HandlerFunc) (*fakeGitLab, *Client) {
	t.Helper()
	f, c := newFake(t, h)
	c.writes = WriteProjectHooks
	return f, c
}
