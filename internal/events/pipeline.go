package events

import (
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Pipeline Hook: a pipeline that finished (failed or success) becomes a
// core.JobPipeline for the CI failure digest (internal/review/pipeline):
//   - a merge request pipeline (the payload names the MR) -> that MR;
//   - a pipeline of the project's main branch (Options.MainBranch) -> the
//     main branch scope (MRIID 0, Pipeline.Main);
//   - any other branch -> MRIID 0: the handler finds the open MRs of the
//     branch whose head is the pipeline's commit.
// Tag pipelines, child pipelines (source parent_pipeline: the parent's own
// event covers them — an MR's head pipeline and a main branch's state are
// the parent's) and every other status (running, canceled, skipped, manual,
// …) are ignored.

// PipelineEvent is a pipeline event (Pipeline Hook) in neutral form (the
// poller lists pipelines through the API and produces the same jobs).
type PipelineEvent struct {
	ProjectID     int64
	ProjectPath   string
	DefaultBranch string
	PipelineID    int64
	Ref           string
	SHA           string
	Status        string
	Tag           bool
	// Source: GitLab's pipeline source (push, schedule, parent_pipeline, …).
	Source string
	// At: when the pipeline reached Status (the hook's finished_at, the
	// API's updated_at; zero = unknown). Part of the dedup identity.
	At time.Time
	// MRIID is set for merge request pipelines (payload merge_request, or a
	// refs/merge-requests/<iid>/head ref).
	MRIID int64
}

// pipelineFinished: the statuses the CI digest acts on.
func pipelineFinished(status string) bool { return status == "failed" || status == "success" }

func (r *Router) pipelineJob(e PipelineEvent) (*core.Job, string) {
	if !r.opts.Pipelines {
		return nil, "pipeline events off (review.ci.enabled: false)"
	}
	if e.ProjectID == 0 || e.PipelineID == 0 {
		return nil, "missing project or pipeline id"
	}
	if e.Tag {
		return nil, "tag pipeline"
	}
	if strings.TrimSpace(e.Source) == "parent_pipeline" {
		return nil, "child pipeline (the parent pipeline's event covers it)"
	}
	if !pipelineFinished(e.Status) {
		return nil, "pipeline " + e.Status
	}
	ref := &core.PipelineRef{ID: e.PipelineID, Ref: e.Ref, SHA: e.SHA, Status: e.Status, DefaultBranch: e.DefaultBranch,
		Source: strings.TrimSpace(e.Source), At: e.At}
	job := &core.Job{Kind: core.JobPipeline, ProjectID: e.ProjectID, ProjectPath: e.ProjectPath, MRIID: e.MRIID,
		HeadSHA: e.SHA, Pipeline: ref}
	if e.MRIID == 0 {
		if e.Ref == "" {
			return nil, "pipeline without ref"
		}
		main := e.DefaultBranch
		if r.opts.MainBranch != nil {
			main = r.opts.MainBranch(e.ProjectPath, e.DefaultBranch)
		}
		ref.Main = main != "" && strings.TrimSpace(e.Ref) == main
	}
	return job, ""
}

// Pipeline returns the job for e, or nil and the reason it is ignored.
func (r *Router) Pipeline(e PipelineEvent) (*core.Job, string) {
	return r.finish(r.pipelineJob(e))
}
