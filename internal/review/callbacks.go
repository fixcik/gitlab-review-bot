package review

// The orchestrator's implementations of the callback interfaces that the
// feature packages declare (dependency inversion: no feature imports
// review). New hands them to the features:
//
//	repoConfigs     env.RepoConfig          .reviewbot.yaml (repoconfig.go)
//	learnReactions  learning.NoteReactions  award emoji (reactions*.go)
//	pipelineHost    pipeline.Host           the queue, the auto-review gate (commands.go)
//
// The verifier's tools (verify.Tools) come from the review's MCP run
// (mcp.Run.VerifyTools), not from here.

import (
	"context"
	"errors"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// repoConfigs is the env.RepoConfig of the features (repoconfig.go).
type repoConfigs struct{ r *Reviewer }

func (c repoConfigs) Effective(ctx context.Context, projectID int64, mr *core.MergeRequest) (config.Effective, error) {
	return c.r.effectiveConfig(ctx, projectID, mr)
}

func (c repoConfigs) Command(ctx context.Context, job core.Job, mr *core.MergeRequest) (config.Effective, bool, error) {
	return c.r.commandConfig(ctx, job, mr)
}

func (c repoConfigs) Auto(ctx context.Context, projectID int64, mr *core.MergeRequest) (config.Effective, bool, error) {
	return c.r.autoConfig(ctx, projectID, mr)
}

func (repoConfigs) NotOptedIn(err error) bool {
	rc := (*repoConfigError)(nil)
	return errors.As(err, &rc) && rc.notOptedIn
}

// learnReactions is the learning.NoteReactions of the learning feature.
type learnReactions struct{ r *Reviewer }

func (x learnReactions) ReactsTo(job core.Job) bool { return x.r.reactsTo(job) }

func (x learnReactions) ThreadReply(ctx context.Context, job core.Job, disc *core.Discussion, idx int) bool {
	return x.r.reactThreadReply(ctx, job, disc, idx)
}

func (x learnReactions) PickedUp(ctx context.Context) { x.r.reactPickedUp(ctx) }

// pipelineHost is the pipeline.Host of the CI features (commands.go).
type pipelineHost struct{ r *Reviewer }

func (h pipelineHost) Enqueue(job core.Job) bool { return h.r.enqueueJob(job) }

func (h pipelineHost) SkipAuto(ctx context.Context, job core.Job, mr *core.MergeRequest) (bool, error) {
	return h.r.skipAuto(ctx, job, mr)
}
