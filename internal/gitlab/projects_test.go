package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestListGroupProjectsDetails(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{"id": 1, "path_with_namespace": "backend/billing", "default_branch": "main", "statistics": map[string]any{"repository_size": 1234}},
			{"id": 2, "path_with_namespace": "backend/empty"},
		})
	})
	got, err := c.ListGroupProjectsDetails(context.Background(), "/backend/")
	if err != nil {
		t.Fatal(err)
	}
	want := []ProjectDetails{
		{ID: 1, PathWithNamespace: "backend/billing", DefaultBranch: "main", RepositorySize: 1234},
		{ID: 2, PathWithNamespace: "backend/empty"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].RawPath != "/api/v4/groups/backend/projects" {
		t.Fatalf("requests %+v", reqs)
	}
	q, _ := url.ParseQuery(reqs[0].Query)
	if q.Get("statistics") != "true" || q.Get("archived") != "false" || q.Get("include_subgroups") != "true" {
		t.Errorf("query %q", reqs[0].Query)
	}
	if _, err := c.ListGroupProjectsDetails(context.Background(), " "); err == nil {
		t.Error("empty group must fail")
	}
}

func TestGetProjectDetails(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == "/api/v4/projects/front%2Fweb" {
			writeJSON(w, map[string]any{"id": 9, "path_with_namespace": "front/web", "default_branch": "main", "statistics": map[string]any{"repository_size": 77}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]any{"message": "404 Project Not Found"})
	})
	got, err := c.GetProjectDetails(context.Background(), "/front/web/")
	if err != nil {
		t.Fatal(err)
	}
	if *got != (ProjectDetails{ID: 9, PathWithNamespace: "front/web", DefaultBranch: "main", RepositorySize: 77}) {
		t.Fatalf("got %+v", got)
	}
	q, _ := url.ParseQuery(f.requests()[0].Query)
	if q.Get("statistics") != "true" || f.requests()[0].Method != http.MethodGet {
		t.Errorf("request %+v", f.requests()[0])
	}
	if _, err := c.GetProjectDetails(context.Background(), "secret/repo"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}
