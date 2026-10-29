package webhook

import (
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/events"
)

// Pipeline Hook payloads: decoded into events.PipelineEvent; which finished
// pipelines become core.JobPipeline is decided by events.Router.Pipeline
// (internal/events/pipeline.go).

type pipelinePayload struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		ID         int64  `json:"id"`
		Ref        string `json:"ref"`
		Tag        bool   `json:"tag"`
		SHA        string `json:"sha"`
		Status     string `json:"status"`
		Source     string `json:"source"`
		FinishedAt string `json:"finished_at"`
	} `json:"object_attributes"`
	MergeRequest *struct {
		IID   int64  `json:"iid"`
		State string `json:"state"`
	} `json:"merge_request"`
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		DefaultBranch     string `json:"default_branch"`
	} `json:"project"`
}

func (p *pipelinePayload) event() events.PipelineEvent {
	a := p.ObjectAttributes
	e := events.PipelineEvent{ProjectID: p.Project.ID, ProjectPath: p.Project.PathWithNamespace, DefaultBranch: p.Project.DefaultBranch,
		PipelineID: a.ID, Ref: a.Ref, SHA: a.SHA, Status: a.Status, Tag: a.Tag, Source: a.Source, At: parseHookTime(a.FinishedAt)}
	if p.MergeRequest != nil && p.MergeRequest.IID > 0 {
		e.MRIID = p.MergeRequest.IID
	}
	return e
}

// parseHookTime reads a webhook timestamp: "2026-10-24 10:05:00 UTC" (the
// Pipeline Hook) or RFC 3339; zero when absent or unreadable.
func parseHookTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
