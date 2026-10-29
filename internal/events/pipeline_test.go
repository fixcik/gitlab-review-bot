package events

import (
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// The poller produces the same jobs through the Router.
func TestRouterPipeline(t *testing.T) {
	r := NewRouter(Options{BotUsername: "reviewbot", Groups: []string{"group"}, Pipelines: true,
		Now: func() time.Time { return fixedNow }})
	j, why := r.Pipeline(PipelineEvent{ProjectID: 42, ProjectPath: "group/api", DefaultBranch: "main", PipelineID: 5,
		Ref: "main", SHA: "abc", Status: "failed"})
	if j == nil || !j.Pipeline.Main || j.MRIID != 0 || j.EnqueuedAt != fixedNow {
		t.Fatalf("%+v %s", j, why)
	}
	j, _ = r.Pipeline(PipelineEvent{ProjectID: 42, ProjectPath: "group/api", DefaultBranch: "main", PipelineID: 6,
		Ref: "refs/merge-requests/7/head", SHA: "abc", Status: "success", MRIID: 7})
	if j == nil || j.Pipeline.Main || j.MRIID != 7 {
		t.Fatalf("%+v", j)
	}
	if j, why := r.Pipeline(PipelineEvent{ProjectID: 42, ProjectPath: "other/api", PipelineID: 6, Ref: "main", Status: "failed"}); j != nil {
		t.Fatalf("outside groups accepted: %s", why)
	}
	// A child pipeline: the parent's event covers it (MR head, main state).
	for _, ref := range []string{"main", "refs/merge-requests/7/head", "feature"} {
		if j, why := r.Pipeline(PipelineEvent{ProjectID: 42, ProjectPath: "group/api", DefaultBranch: "main", PipelineID: 8,
			Ref: ref, SHA: "abc", Status: "failed", Source: "parent_pipeline", MRIID: gitlabMRIID(ref)}); j != nil || !strings.Contains(why, "child pipeline") {
			t.Fatalf("%s: child pipeline accepted: %+v %s", ref, j, why)
		}
	}
}

func gitlabMRIID(ref string) int64 {
	if strings.HasPrefix(ref, "refs/merge-requests/") {
		return 7
	}
	return 0
}
