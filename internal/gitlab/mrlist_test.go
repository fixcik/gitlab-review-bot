package gitlab

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestListUpdatedMRs(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{"project_id": 7, "iid": 3, "title": "Draft: feat: x", "state": "opened", "draft": true, "sha": "abc",
				"created_at": "2026-10-24T10:00:00Z", "updated_at": "2026-10-24T11:00:00.123Z",
				"references": map[string]any{"full": "team/sub/api!3"}},
			{"project_id": 8, "iid": 1, "state": "merged", "work_in_progress": true, "references": map[string]any{"full": "team/web!1"}},
		})
	})
	since := time.Date(2026, 10, 24, 12, 0, 0, 0, time.FixedZone("ICT", 7*3600))
	got, err := c.ListUpdatedMRs(context.Background(), "/team/sub/", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ProjectPath != "team/sub/api" || got[0].IID != 3 || !got[0].Draft || got[0].SHA != "abc" ||
		got[0].Title != "Draft: feat: x" || got[0].UpdatedAt.Format(time.RFC3339Nano) != "2026-10-24T11:00:00.123Z" ||
		got[1].ProjectPath != "team/web" || !got[1].Draft || got[1].State != "merged" {
		t.Fatalf("got %+v", got)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].RawPath != "/api/v4/groups/team%2Fsub/merge_requests" {
		t.Fatalf("requests %+v", reqs)
	}
	q, _ := url.ParseQuery(reqs[0].Query)
	if q.Get("updated_after") != "2026-10-24T05:00:00Z" || q.Get("scope") != "all" || q.Get("order_by") != "updated_at" ||
		q.Get("sort") != "asc" || q.Has("state") {
		t.Errorf("query %q", reqs[0].Query)
	}

	// No group: every visible MR; zero since: all open MRs.
	if _, err := c.ListUpdatedMRs(context.Background(), "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	reqs = f.requests()
	last := reqs[len(reqs)-1]
	q, _ = url.ParseQuery(last.Query)
	if last.RawPath != "/api/v4/merge_requests" || q.Get("state") != "opened" || q.Has("updated_after") || q.Get("scope") != "all" {
		t.Errorf("all-MRs request %+v", last)
	}
}

func TestListOpenMRs(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{"project_id": 9, "iid": 12, "title": "feat: history", "description": "PROJ-11", "state": "opened", "draft": false,
				"source_branch": "PROJ-11-history", "target_branch": "main", "web_url": "https://gl/x/-/merge_requests/12", "sha": "abc"},
			{"project_id": 9, "iid": 13, "work_in_progress": true, "target_branch": "main"},
			{"project_id": 9, "iid": 14},
		})
	})
	got, err := c.ListOpenMRs(context.Background(), "/backend/orders/", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ProjectID != 9 || got[0].IID != 12 || got[0].SourceBranch != "PROJ-11-history" || got[0].TargetBranch != "main" ||
		got[0].Description != "PROJ-11" || got[0].DiffRefs.HeadSHA != "abc" || !got[1].Draft {
		t.Fatalf("got %+v", got)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].RawPath != "/api/v4/projects/backend%2Forders/merge_requests" {
		t.Fatalf("requests %+v", reqs)
	}
	q, _ := url.ParseQuery(reqs[0].Query)
	if q.Get("state") != "opened" || q.Get("per_page") != "2" || q.Get("page") != "1" || q.Get("sort") != "desc" {
		t.Errorf("query %q", reqs[0].Query)
	}
	if _, err := c.ListOpenMRs(context.Background(), " ", 5); err == nil {
		t.Error("empty path must fail")
	}
}
