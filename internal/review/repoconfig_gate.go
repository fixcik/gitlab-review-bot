package review

// The review gate on the repository config and config access for commands and automatic jobs.

import (
	"context"
	"errors"
	"fmt"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

// cfgGate is the outcome of the repository config step of a review.
type cfgGate struct {
	eff config.Effective
	// summaryLine goes to the top of the review summary ("" = none).
	summaryLine string
}

// reviewConfigGate runs step 3 of a review: reads the target and head
// configs, keeps the config note, and decides. ok=false means the job is
// over (skipped, replied); err is a GitLab failure.
func (r *Reviewer) reviewConfigGate(ctx context.Context, job core.Job, mr *core.MergeRequest) (g cfgGate, ok bool, err error) {
	log := r.env.Log.With("mr", job.MRKey(), "kind", job.Kind)
	manual := job.Kind.Manual()
	target, err := r.readRepoConfig(ctx, job.ProjectID, mr.TargetBranch)
	if err != nil {
		return g, false, err
	}
	optedIn := r.optedIn(target)
	head, herr := r.headRepoConfig(ctx, job.ProjectID, mr, target)
	if herr != nil {
		log.Warn("review: read .reviewbot.yaml of the mr head", "err", herr)
	}
	// Not opted in and the MR does not bring a config: nothing to say, no
	// note to look for (the cheap path for every event of such projects).
	if optedIn || head != nil {
		r.syncConfigNote(ctx, job, mr, target, head, optedIn)
	}
	switch {
	case target.found && len(target.issues) > 0:
		log.Warn("review: invalid .reviewbot.yaml on the target branch, skipping", "branch", mr.TargetBranch, "errors", len(target.issues))
		r.noteRepoConfigProblem(ctx, mr.TargetBranch, target.issues)
		r.skip(SkipRepoConfigInvalid)
		if manual {
			jobctx.Denied(ctx) // 🚫 next to the reply (reactions.go)
			return g, false, r.env.Reply(ctx, job, repoConfigMessage(&repoConfigError{err: target.issues, branch: mr.TargetBranch}))
		}
		return g, false, nil
	case !optedIn:
		log.Info("review: repository not opted in (no .reviewbot.yaml on the target branch), skipping", "branch", mr.TargetBranch)
		r.skip(SkipNoRepoConfig)
		if manual {
			jobctx.Denied(ctx)
			return g, false, r.env.Reply(ctx, job, fmt.Sprintf(msgNotOptedIn, cfgCode(mr.TargetBranch)))
		}
		return g, false, nil
	}
	// The target branch's config is fine now: an earlier repo_config_invalid
	// of this project is fixed.
	problems.Resolve(ctx, problems.RepoConfigInvalid, config.RepoConfigPath)
	g.eff = config.Merge(r.env.Cfg, target.repo)
	if head != nil && !head.valid() {
		g.summaryLine = fmt.Sprintf("⚠️ **`.reviewbot.yaml` в этом MR невалиден** (%d %s) — после мержа бот перестанет ревьюить этот репозиторий; подробности — в отдельной заметке бота.",
			len(head.issues), mdtext.Plural(len(head.issues), "ошибка", "ошибки", "ошибок"))
	}
	return g, true, nil
}

// skip counts a review skipped because of the repository config.
func (r *Reviewer) skip(reason string) {
	if r.onSkip != nil {
		r.onSkip(reason)
	}
}

// noteRepoConfigProblem records repo_config_invalid (one fingerprint per
// project: the subject is the file name).
func (r *Reviewer) noteRepoConfigProblem(ctx context.Context, branch string, issues config.RepoErrors) {
	parts := make([]string, 0, len(issues))
	for _, is := range issues {
		parts = append(parts, is.String())
	}
	jobctx.BotProblem(ctx, problems.RepoConfigInvalid, config.RepoConfigPath, "ветка "+branch+": "+strings.Join(parts, "; "))
}

// denyRepoConfig answers a command refused because of the repository config
// (not opted in, or invalid on the target branch: then the MR note and the
// problem are kept up to date as well). The command's note gets 🚫 instead
// of ✅ (reactions.go).
func (r *Reviewer) denyRepoConfig(ctx context.Context, job core.Job, mr *core.MergeRequest, rc *repoConfigError) error {
	jobctx.Denied(ctx)
	if !rc.notOptedIn && mr != nil {
		issues := config.RepoIssuesOf(rc.err)
		r.noteRepoConfigProblem(ctx, rc.branch, issues)
		target := cfgFile{found: true, issues: issues}
		if t, err := r.readRepoConfig(ctx, job.ProjectID, mr.TargetBranch); err == nil {
			target = t
		}
		head, err := r.headRepoConfig(ctx, job.ProjectID, mr, target)
		if err != nil {
			r.env.Log.Warn("review: read .reviewbot.yaml of the mr head", "mr", job.MRKey(), "err", err)
		}
		r.syncConfigNote(ctx, job, mr, target, head, true)
	}
	return r.env.Reply(ctx, job, repoConfigMessage(rc))
}

// commandConfig is effectiveConfig for a command: a repository config that
// refuses it is answered (denyRepoConfig) and ok is false; ok is false on
// other errors too (err set).
func (r *Reviewer) commandConfig(ctx context.Context, job core.Job, mr *core.MergeRequest) (eff config.Effective, ok bool, err error) {
	eff, err = r.effectiveConfig(ctx, job.ProjectID, mr)
	var rc *repoConfigError
	switch {
	case err == nil:
		return eff, true, nil
	case errors.As(err, &rc):
		return eff, false, r.denyRepoConfig(ctx, job, mr, rc)
	}
	return eff, false, err
}

// autoConfig is effectiveConfig for an automatic run (nothing to answer): a
// repository config that refuses it skips the run silently (ok false, err
// nil); ok is false on other errors too (err set).
func (r *Reviewer) autoConfig(ctx context.Context, projectID int64, mr *core.MergeRequest) (eff config.Effective, ok bool, err error) {
	eff, err = r.effectiveConfig(ctx, projectID, mr)
	var rc *repoConfigError
	switch {
	case err == nil:
		return eff, true, nil
	case errors.As(err, &rc):
		return eff, false, nil
	}
	return eff, false, err
}

// syncConfigNote creates, updates or leaves the config note.
