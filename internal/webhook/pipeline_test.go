package webhook

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
)

// pipelineHook builds a realistic Pipeline Hook payload (GitLab 17).
func pipelineHook(id int64, ref, status string, tag bool, mr string) string {
	if mr == "" {
		mr = "null"
	}
	return fmt.Sprintf(`{
  "object_kind": "pipeline",
  "object_attributes": {
    "id": %d, "iid": 3, "name": null, "ref": %q, "tag": %t,
    "sha": "abc123def4567890", "before_sha": "0000000000000000", "source": "push",
    "status": %q, "detailed_status": "failed", "stages": ["build", "test"],
    "created_at": "2026-10-24 10:00:00 UTC", "finished_at": "2026-10-24 10:05:00 UTC", "duration": 300,
    "variables": []
  },
  "merge_request": %s,
  "user": {"id": 5, "name": "Alice", "username": "alice"},
  "project": {"id": 42, "name": "api", "path_with_namespace": "group/sub/api", "default_branch": "main",
    "web_url": "https://gitlab.example.com/group/sub/api"},
  "commit": {"id": "abc123def4567890", "message": "fix", "author": {"name": "Alice", "email": "[REDACTED]"}},
  "builds": [{"id": 380, "stage": "test", "name": "tests", "status": "failed", "allow_failure": false}]
}`, id, ref, tag, status, mr)
}

func newPipelineHandler(rec *recorder, on bool, main func(string, string) string) http.Handler {
	return NewHandler(Options{
		Secret:  secret,
		Enqueue: rec.enqueue,
		Router:  events.NewRouter(events.Options{BotUsername: "reviewbot", Now: func() time.Time { return fixedNow }, Groups: []string{"group"}, Pipelines: on, MainBranch: main}),
	})
}

func TestPipelineHook(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		main      func(string, string) string
		want      bool
		wantMR    int64
		wantMain  bool
		reasonSub string
	}{
		{name: "main failed", body: pipelineHook(31, "main", "failed", false, ""), want: true, wantMain: true},
		{name: "main success", body: pipelineHook(31, "main", "success", false, ""), want: true, wantMain: true},
		{name: "configured main branch", body: pipelineHook(31, "release", "failed", false, ""),
			main: func(p, d string) string { return "release" }, want: true, wantMain: true},
		{name: "branch failed", body: pipelineHook(32, "feature", "failed", false, ""), want: true},
		{name: "mr pipeline", body: pipelineHook(33, "refs/merge-requests/7/head", "failed", false,
			`{"id": 1, "iid": 7, "title": "x", "source_branch": "feature", "target_branch": "main", "state": "opened"}`), want: true, wantMR: 7},
		{name: "running", body: pipelineHook(34, "main", "running", false, ""), reasonSub: "pipeline running"},
		{name: "canceled", body: pipelineHook(34, "main", "canceled", false, ""), reasonSub: "pipeline canceled"},
		{name: "tag", body: pipelineHook(35, "v1.0", "failed", true, ""), reasonSub: "tag pipeline"},
		{name: "child pipeline", body: strings.Replace(pipelineHook(36, "main", "failed", false, ""), `"source": "push"`, `"source": "parent_pipeline"`, 1),
			reasonSub: "child pipeline"},
	} {
		rec := &recorder{}
		w := do(newPipelineHandler(rec, true, tc.main), http.MethodPost, "Pipeline Hook", secret, tc.body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body)
		}
		if !tc.want {
			if len(rec.jobs) != 0 || !strings.Contains(w.Body.String(), tc.reasonSub) {
				t.Errorf("%s: jobs %v, body %q", tc.name, rec.jobs, w.Body)
			}
			continue
		}
		if len(rec.jobs) != 1 {
			t.Fatalf("%s: jobs %v (%s)", tc.name, rec.jobs, w.Body)
		}
		j := rec.jobs[0]
		if j.Kind != core.JobPipeline || j.ProjectID != 42 || j.ProjectPath != "group/sub/api" || j.MRIID != tc.wantMR ||
			j.Pipeline == nil || j.Pipeline.Main != tc.wantMain || j.Pipeline.SHA != "abc123def4567890" || j.HeadSHA != "abc123def4567890" ||
			j.Pipeline.DefaultBranch != "main" || j.EnqueuedAt != fixedNow || j.Pipeline.Source != "push" ||
			!j.Pipeline.At.Equal(time.Date(2026, 10, 24, 10, 5, 0, 0, time.UTC)) {
			t.Errorf("%s: %+v %+v", tc.name, j, j.Pipeline)
		}
	}
}

func TestPipelineHookOffAndGroups(t *testing.T) {
	rec := &recorder{}
	w := do(newPipelineHandler(rec, false, nil), http.MethodPost, "Pipeline Hook", secret, pipelineHook(31, "main", "failed", false, ""))
	if len(rec.jobs) != 0 || !strings.Contains(w.Body.String(), "review.ci.enabled") {
		t.Fatalf("off: %v %q", rec.jobs, w.Body)
	}
	body := strings.Replace(pipelineHook(31, "main", "failed", false, ""), "group/sub/api", "other/api", 1)
	w = do(newPipelineHandler(rec, true, nil), http.MethodPost, "Pipeline Hook", secret, body)
	if len(rec.jobs) != 0 || !strings.Contains(w.Body.String(), "outside the configured groups") {
		t.Fatalf("groups: %v %q", rec.jobs, w.Body)
	}
	w = do(newPipelineHandler(rec, true, nil), http.MethodPost, "Pipeline Hook", secret, "{")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", w.Code)
	}
}
