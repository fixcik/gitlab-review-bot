package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestAddReviewerKeepsExisting(t *testing.T) {
	var put map[string]any
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]any{"iid": 7, "reviewers": []map[string]any{{"id": 3, "username": "ann"}}})
		case http.MethodPut:
			json.NewDecoder(r.Body).Decode(&put)
			writeJSON(w, map[string]any{"iid": 7})
		}
	})
	c.writes = ServiceWrites
	added, err := c.AddReviewer(context.Background(), 42, 7, 99)
	if err != nil || !added {
		t.Fatalf("added=%v err=%v", added, err)
	}
	ids, _ := put["reviewer_ids"].([]any)
	if len(put) != 1 || len(ids) != 2 || ids[0].(float64) != 3 || ids[1].(float64) != 99 {
		t.Fatalf("PUT body must contain only reviewer_ids [3 99]: %v", put)
	}
	_ = f
}

func TestAddReviewerAlreadyThere(t *testing.T) {
	puts := 0
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
		}
		writeJSON(w, map[string]any{"reviewers": []map[string]any{{"id": 99}}})
	})
	c.writes = ServiceWrites
	added, err := c.AddReviewer(context.Background(), 42, 7, 99)
	if err != nil || added || puts != 0 {
		t.Fatalf("added=%v err=%v puts=%d", added, err, puts)
	}
}

func TestReviewerAndResolvePolicy(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"reviewers": []map[string]any{}})
	})
	// The bot's own client (MR comments) may not change reviewers.
	if _, err := c.AddReviewer(context.Background(), 42, 7, 99); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("comments-only client changed reviewers: %v", err)
	}
	// The service client may resolve and set reviewers, but not post notes.
	c.writes = ServiceWrites
	if err := c.ResolveDiscussion(context.Background(), 42, 7, "abc", true); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := c.CreateNote(context.Background(), 42, 7, "hi"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("service client posted a note: %v", err)
	}
	// Only a reviewers-only body may update the MR.
	if writeAllowedWithBody(ServiceWrites, http.MethodPut, "/projects/1/merge_requests/2", map[string]any{"reviewer_ids": []int{1}, "state_event": "close"}) {
		t.Error("arbitrary MR update allowed")
	}
	if !writeAllowedWithBody(ServiceWrites, http.MethodPut, "/projects/1/merge_requests/2", reviewersUpdate{ReviewerIDs: []int64{1}}) {
		t.Error("reviewers update refused")
	}
	if writeAllowedWithBody(ServiceWrites, http.MethodPut, "/projects/1/merge_requests/2/merge", reviewersUpdate{}) {
		t.Error("merge endpoint allowed")
	}
	if writeAllowedWithBody(WriteMRComments, http.MethodPut, "/projects/1/merge_requests/2", reviewersUpdate{}) {
		t.Error("comments policy allowed reviewer update")
	}
}
