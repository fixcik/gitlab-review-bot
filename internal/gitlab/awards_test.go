package gitlab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestAwards(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.EscapedPath() {
		case "GET /api/v4/projects/42/merge_requests/7/notes/9/award_emoji":
			io.WriteString(w, `[{"id":1,"name":"eyes","user":{"id":99,"username":"reviewbot"}},{"id":2,"name":"heart","user":{"id":5,"username":"alice"}}]`)
		case "GET /api/v4/projects/42/merge_requests/7/award_emoji":
			io.WriteString(w, `[]`)
		case "POST /api/v4/projects/42/merge_requests/7/notes/9/award_emoji", "POST /api/v4/projects/42/merge_requests/7/award_emoji":
			io.WriteString(w, `{"id":3,"name":"tada","user":{"id":99,"username":"reviewbot"}}`)
		case "DELETE /api/v4/projects/42/merge_requests/7/notes/9/award_emoji/1":
			w.WriteHeader(http.StatusNoContent)
		case "DELETE /api/v4/projects/42/merge_requests/7/award_emoji/4":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	if _, err := c.AddAward(ctx, 42, 7, 9, "eyes"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("comments-only client awarded emoji: %v", err)
	}
	c.writes = BotWrites
	got, err := c.ListAwards(ctx, 42, 7, 9)
	if err != nil || len(got) != 2 || got[0] != (core.Award{ID: 1, Name: "eyes", UserID: 99, Username: "reviewbot"}) || got[1].UserID != 5 {
		t.Fatalf("ListAwards = %+v, %v", got, err)
	}
	if got, err := c.ListAwards(ctx, 42, 7, 0); err != nil || len(got) != 0 {
		t.Fatalf("MR awards = %+v, %v", got, err)
	}
	a, err := c.AddAward(ctx, 42, 7, 9, "tada")
	if err != nil || a.ID != 3 || a.Name != "tada" {
		t.Fatalf("AddAward = %+v, %v", a, err)
	}
	if _, err := c.AddAward(ctx, 42, 7, 0, "tada"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteAward(ctx, 42, 7, 9, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteAward(ctx, 42, 7, 0, 4); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("delete 403: %v", err)
	}
	var posted []string
	for _, r := range f.requests() {
		if r.Method == http.MethodPost {
			posted = append(posted, r.Body)
		}
	}
	if len(posted) != 2 || posted[0] != `{"name":"tada"}` {
		t.Errorf("POST bodies %q", posted)
	}
}

func TestAwardWritePolicy(t *testing.T) {
	cases := []struct {
		p      WritePolicy
		method string
		path   string
		want   bool
	}{
		{BotWrites, "POST", "/projects/1/merge_requests/2/award_emoji", true},
		{BotWrites, "POST", "/projects/1/merge_requests/2/notes/3/award_emoji", true},
		{BotWrites, "DELETE", "/projects/1/merge_requests/2/notes/3/award_emoji/4", true},
		{BotWrites, "DELETE", "/projects/1/merge_requests/2/award_emoji/4", true},
		{BotWrites, "DELETE", "/projects/1/merge_requests/2/award_emoji", false},
		{BotWrites, "POST", "/projects/1/merge_requests/2/award_emoji/4", false},
		{BotWrites, "PUT", "/projects/1/merge_requests/2/award_emoji/4", false},
		{BotWrites, "POST", "/projects/1/issues/2/award_emoji", false},
		{BotWrites, "DELETE", "/projects/1/merge_requests/2/notes/3", false},
		{WriteMRComments, "POST", "/projects/1/merge_requests/2/award_emoji", false},
		{ServiceWrites, "POST", "/projects/1/merge_requests/2/notes/3/award_emoji", false},
	}
	for _, c := range cases {
		if got := writeAllowed(c.p, c.method, c.path); got != c.want {
			t.Errorf("writeAllowed(%d, %s %s) = %v, want %v", c.p, c.method, c.path, got, c.want)
		}
	}
}
