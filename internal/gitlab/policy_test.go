package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestWriteAllowed(t *testing.T) {
	cases := []struct {
		p      WritePolicy
		method string
		path   string
		want   bool
	}{
		{WriteMRComments, "POST", "/projects/1/merge_requests/2/notes", true},
		{WriteMRComments, "PUT", "/projects/1/merge_requests/2/notes/3", true},
		{WriteMRComments, "POST", "/projects/1/merge_requests/2/discussions/abc123/notes", true},
		{WriteMRComments, "PUT", "/projects/1/merge_requests/2/discussions/abc123", true},
		{WriteMRComments, "POST", "/projects/1/merge_requests/2/draft_notes", true},
		{WriteMRComments, "POST", "/projects/1/merge_requests/2/draft_notes/bulk_publish", true},
		{WriteMRComments, "DELETE", "/projects/1/merge_requests/2/draft_notes/5", true},
		{WriteMRComments, "DELETE", "/projects/1/merge_requests/2/notes/3", false},
		// Everything that changes the repository or the MR itself is refused.
		{WriteMRComments, "PUT", "/projects/1/merge_requests/2/merge", false},
		{WriteMRComments, "POST", "/projects/1/merge_requests/2/approve", false},
		{WriteMRComments, "PUT", "/projects/1/merge_requests/2", false},
		{WriteMRComments, "POST", "/projects/1/repository/commits", false},
		{WriteMRComments, "POST", "/projects/1/repository/files/a.go", false},
		{WriteMRComments, "PUT", "/projects/1", false},
		{WriteMRComments, "POST", "/projects/1/hooks", false},
		{WriteMRComments, "POST", "/projects/1/merge_requests/2/notes/../merge", false},
		{WriteProjectHooks, "POST", "/projects/1/hooks", true},
		{WriteProjectHooks, "PUT", "/projects/1/hooks/9", true},
		{WriteProjectHooks, "DELETE", "/projects/1/hooks/9", false},
		{WriteProjectHooks, "POST", "/projects/1/merge_requests/2/notes", false},
	}
	for _, c := range cases {
		if got := writeAllowed(c.p, c.method, c.path); got != c.want {
			t.Errorf("writeAllowed(%d, %s %s) = %v, want %v", c.p, c.method, c.path, got, c.want)
		}
	}
}

func TestDisallowedWriteNeverLeavesProcess(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()
	// Hooks-only client must not be able to post MR notes.
	c, err := New(srv.URL, "token-12345678", WithWrites(WriteProjectHooks))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateNote(context.Background(), 1, 2, "hi")
	if !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("request reached the server")
	}
	// Default client may not create hooks.
	d, _ := New(srv.URL, "token-12345678")
	if _, err := d.AddProjectHook(context.Background(), 1, HookSpec{URL: "https://x/webhook"}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("default client created a hook: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("request reached the server")
	}
}

func TestForbiddenMapsToErrForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "token-12345678", WithRetry(0, 0))
	err := c.ResolveDiscussion(context.Background(), 1, 2, "abc", true)
	if !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}
