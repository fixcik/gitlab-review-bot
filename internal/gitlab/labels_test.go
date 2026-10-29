package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestUpdateMRLabels(t *testing.T) {
	var put map[string]any
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		put = nil
		json.NewDecoder(r.Body).Decode(&put)
		writeJSON(w, map[string]any{"iid": 7, "state": "opened", "labels": []string{"backend", "type::bug"}})
	})
	c.writes = BotWrites
	labels, err := c.UpdateMRLabels(context.Background(), 42, 7, []string{"backend", "type::bug"}, []string{"stale"})
	if err != nil || strings.Join(labels, ",") != "backend,type::bug" {
		t.Fatalf("labels=%v err=%v", labels, err)
	}
	if len(put) != 2 || put["add_labels"] != "backend,type::bug" || put["remove_labels"] != "stale" {
		t.Fatalf("PUT body must carry only add_labels/remove_labels: %v", put)
	}
	if req := f.req[0]; req.Method != http.MethodPut || req.RawPath != "/api/v4/projects/42/merge_requests/7" {
		t.Fatalf("request %+v", req)
	}
	// Only adds: no remove_labels key at all.
	if _, err := c.UpdateMRLabels(context.Background(), 42, 7, []string{"a"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := put["remove_labels"]; ok || len(put) != 1 {
		t.Fatalf("body %v", put)
	}
	// Names with commas or empty, or nothing to do: refused before sending.
	for _, add := range [][]string{{"a,b"}, {" "}, nil} {
		if _, err := c.UpdateMRLabels(context.Background(), 42, 7, add, nil); err == nil {
			t.Errorf("%q accepted", add)
		}
	}
	if len(f.req) != 2 {
		t.Fatalf("requests %d", len(f.req))
	}
}

func TestLabelsPolicy(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"labels": []string{}})
	})
	// The default (comments-only) client may not change labels.
	if _, err := c.UpdateMRLabels(context.Background(), 42, 7, []string{"a"}, nil); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("comments-only client changed labels: %v", err)
	}
	for _, p := range []WritePolicy{BotWrites, ServiceWrites} {
		c.writes = p
		if _, err := c.UpdateMRLabels(context.Background(), 42, 7, []string{"a"}, nil); err != nil {
			t.Fatalf("policy %b: %v", p, err)
		}
	}
	// The labels body type is allowed on the MR itself only, and a labels
	// client cannot send reviewers or anything else.
	if writeAllowedWithBody(BotWrites, http.MethodPut, "/projects/1/merge_requests/2/merge", labelsUpdate{AddLabels: "a"}) {
		t.Fatal("labels body allowed on /merge")
	}
	if writeAllowedWithBody(BotWrites, http.MethodPut, "/projects/1/merge_requests/2", reviewersUpdate{ReviewerIDs: []int64{1}}) {
		t.Fatal("bot client may change reviewers")
	}
	if writeAllowedWithBody(BotWrites, http.MethodPut, "/projects/1/merge_requests/2", map[string]any{"add_labels": "a", "state_event": "close"}) {
		t.Fatal("free-form body allowed")
	}
}

func TestMRLabelsAndProjectLabels(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/labels") {
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Next-Page", "2")
				writeJSON(w, []map[string]any{{"id": 1, "name": "backend"}})
				return
			}
			writeJSON(w, []map[string]any{{"id": 2, "name": "type::bug"}})
			return
		}
		writeJSON(w, map[string]any{"state": "merged", "labels": []string{"x"}})
	})
	state, labels, err := c.MRLabels(context.Background(), 42, 7)
	if err != nil || state != "merged" || strings.Join(labels, ",") != "x" {
		t.Fatalf("%s %v %v", state, labels, err)
	}
	names, err := c.ListProjectLabels(context.Background(), 42)
	if err != nil || strings.Join(names, ",") != "backend,type::bug" {
		t.Fatalf("names %v err %v", names, err)
	}
	q := f.req[1].Query
	if f.req[1].RawPath != "/api/v4/projects/42/labels" || !strings.Contains(q, "include_ancestor_groups=true") {
		t.Fatalf("request %+v", f.req[1])
	}
}
