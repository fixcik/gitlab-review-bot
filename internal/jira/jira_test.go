package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "jira-pat-secret-token"

type fakeJira struct {
	mu     sync.Mutex
	paths  []string
	auth   []string
	fields []string
	issues map[string]map[string]any // key -> fields
	// comments served by the /comment endpoint
	tail map[string][]map[string]any
}

func newFakeJira(t *testing.T) (*fakeJira, *httptest.Server) {
	t.Helper()
	f := &fakeJira{issues: map[string]map[string]any{}, tail: map[string][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.fields = append(f.fields, r.URL.Query().Get("fields"))
		f.mu.Unlock()
		const prefix = "/ctx/rest/api/2/issue/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		if key, ok := strings.CutSuffix(rest, "/comment"); ok {
			if r.URL.Query().Get("startAt") != "23" || r.URL.Query().Get("maxResults") != "2" {
				http.Error(w, "bad paging", http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"comments": f.tail[key], "total": 25, "startAt": 23})
			return
		}
		fields, ok := f.issues[rest]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"errorMessages": []string{"Issue Does Not Exist"}})
			return
		}
		writeJSON(w, map[string]any{"key": rest, "fields": fields})
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func comment(author, body string) map[string]any {
	return map[string]any{"author": map[string]any{"displayName": author, "name": "u"}, "created": "2026-10-20T10:00:00.000+0300", "body": body}
}

func newClient(t *testing.T, url string, opts ...Option) *Client {
	t.Helper()
	c, err := New(url+"/ctx/", testToken, append([]Option{WithRetry(3, time.Millisecond)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGetIssueFull(t *testing.T) {
	f, srv := newFakeJira(t)
	long := strings.Repeat("я", MaxTextRunes+100)
	f.issues["PROJ-10"] = map[string]any{
		"summary": "  Кэш\nдля API ", "description": long,
		"status": map[string]any{"name": "In Progress"}, "issuetype": map[string]any{"name": "Sub-task"},
		"comment": map[string]any{"comments": []any{comment("Иван", "first"), comment("Пётр", "second"), comment("Анна", "third")}, "total": 3, "startAt": 0, "maxResults": 50},
		"parent":  map[string]any{"key": "PROJ-9", "fields": map[string]any{"summary": "Parent"}},
	}
	f.issues["PROJ-9"] = map[string]any{"summary": "Родитель", "description": "desc of parent", "customfield_10008": "PROJ-1"}
	f.issues["PROJ-1"] = map[string]any{"summary": "Эпик", "description": "epic desc"}

	c := newClient(t, srv.URL, WithCommentsLimit(2))
	is, err := c.GetIssue(context.Background(), "PROJ-10")
	if err != nil {
		t.Fatal(err)
	}
	if is.Key != "PROJ-10" || is.URL != srv.URL+"/ctx/browse/PROJ-10" || is.Summary != "Кэш для API" ||
		is.Status != "In Progress" || is.Type != "Sub-task" {
		t.Fatalf("issue: %+v", is)
	}
	if r := []rune(is.Description); len(r) > MaxTextRunes+20 || !strings.HasSuffix(is.Description, "(обрезано)") {
		t.Fatalf("description not truncated: %d runes", len(r))
	}
	if len(is.Comments) != 2 || is.Comments[0].Body != "second" || is.Comments[1].Author != "Анна" || is.Comments[1].Created.IsZero() {
		t.Fatalf("comments: %+v", is.Comments)
	}
	if is.Parent == nil || is.Parent.Key != "PROJ-9" || is.Parent.Summary != "Родитель" || is.Parent.Description != "desc of parent" {
		t.Fatalf("parent: %+v", is.Parent)
	}
	if is.EpicKey != "PROJ-1" || is.Epic == nil || is.Epic.Summary != "Эпик" || is.Epic.URL != srv.URL+"/ctx/browse/PROJ-1" {
		t.Fatalf("epic: %q %+v", is.EpicKey, is.Epic)
	}
	for _, a := range f.auth {
		if a != "Bearer "+testToken {
			t.Fatalf("auth header %q", a)
		}
	}
	if f.fields[0] != "summary,description,status,issuetype,parent,comment,customfield_10008" {
		t.Fatalf("fields param: %q", f.fields[0])
	}
}

func TestGetIssueEpicLinkAndLatestComments(t *testing.T) {
	f, srv := newFakeJira(t)
	f.issues["PROJ-5"] = map[string]any{
		"summary": "Story", "customfield_123": "PROJ-2",
		"comment": map[string]any{"comments": []any{comment("a", "old1")}, "total": 25, "startAt": 0, "maxResults": 1},
	}
	f.tail["PROJ-5"] = []map[string]any{comment("b", "new1"), comment("c", "new2")}
	c := newClient(t, srv.URL, WithEpicLinkField("customfield_123"), WithCommentsLimit(2))
	is, err := c.GetIssue(context.Background(), "PROJ-5")
	if err != nil {
		t.Fatal(err)
	}
	if len(is.Comments) != 2 || is.Comments[0].Body != "new1" {
		t.Fatalf("latest comments: %+v", is.Comments)
	}
	if is.EpicKey != "PROJ-2" || is.Epic == nil || is.Epic.Summary != "" {
		// Epic PROJ-2 does not exist: brief with key only, not an error.
		t.Fatalf("epic: %+v", is.Epic)
	}
	if is.Parent != nil {
		t.Fatalf("parent: %+v", is.Parent)
	}
}

func TestGetIssueNoCommentsNoEpic(t *testing.T) {
	f, srv := newFakeJira(t)
	f.issues["PROJ-5"] = map[string]any{"summary": "Story"}
	c := newClient(t, srv.URL, WithEpicLinkField(""), WithCommentsLimit(0))
	if _, err := c.GetIssue(context.Background(), "PROJ-5"); err != nil {
		t.Fatal(err)
	}
	if f.fields[0] != "summary,description,status,issuetype,parent" {
		t.Fatalf("fields: %q", f.fields[0])
	}
}

func TestErrorsMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrNotFound},
		{http.StatusUnauthorized, ErrForbidden},
		{http.StatusForbidden, ErrForbidden},
	} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(tc.status)
			fmt.Fprintf(w, `{"errorMessages":["token %s rejected"]}`, testToken)
		}))
		c := newClient(t, srv.URL)
		_, err := c.GetIssue(context.Background(), "PROJ-1")
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Fatalf("%d: err = %v", tc.status, err)
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatalf("token leaked: %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("%d must not be retried: %d calls", tc.status, calls.Load())
		}
	}
}

func TestRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			writeJSON(w, map[string]any{"key": "PROJ-1", "fields": map[string]any{"summary": "ok"}})
		}
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	is, err := c.GetIssue(context.Background(), "PROJ-1")
	if err != nil || is.Summary != "ok" || calls.Load() != 3 {
		t.Fatalf("is=%+v err=%v calls=%d", is, err, calls.Load())
	}

	calls.Store(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv2.Close()
	c2 := newClient(t, srv2.URL)
	if _, err := c2.GetIssue(context.Background(), "PROJ-1"); err == nil || calls.Load() != 4 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestBodyLimitAndInvalidKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"key":"PROJ-1","fields":{"summary":"`))
		w.Write([]byte(strings.Repeat("a", maxBodyBytes)))
		w.Write([]byte(`"}}`))
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	if _, err := c.GetIssue(context.Background(), "PROJ-1"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.GetIssue(context.Background(), "../../admin"); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New("jira.example.com", "t"); err == nil {
		t.Fatal("want error for missing scheme")
	}
	if _, err := New("https://jira.example.com", ""); err == nil {
		t.Fatal("want error for empty token")
	}
	if _, err := New("https://jira.example.com", "t", WithEpicLinkField("a,b")); err == nil {
		t.Fatal("want error for bad field")
	}
	c, err := New("https://jira.example.com/", "t")
	if err != nil || c.BrowseURL("PROJ-1") != "https://jira.example.com/browse/PROJ-1" {
		t.Fatalf("browse: %v", err)
	}
}
