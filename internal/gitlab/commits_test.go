package gitlab

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestListMRCommitsPagination(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/5/merge_requests/3/commits" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			writeJSON(w, []map[string]string{
				{"id": "c2", "title": "fix: second (PROJ-2)", "message": "fix: second (PROJ-2)\n\nbody"},
			})
		case "2":
			writeJSON(w, []map[string]string{
				{"id": "c1", "title": "feat: first", "message": "feat: first\n"},
			})
		default:
			http.Error(w, "bad page", http.StatusBadRequest)
		}
	})
	got, err := c.ListMRCommits(context.Background(), 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.Commit{
		{SHA: "c2", Title: "fix: second (PROJ-2)", Message: "fix: second (PROJ-2)\n\nbody"},
		{SHA: "c1", Title: "feat: first", Message: "feat: first\n"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commit %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[0].Token != testToken {
		t.Fatalf("requests: %+v", reqs)
	}
}

func TestListMRCommitsNotFound(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	if _, err := c.ListMRCommits(context.Background(), 5, 3); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
