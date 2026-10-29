package env

// The code before the merge request, for Claude (core.RunRequest.BaseDir).
//
// Claude has no Bash and therefore no git: instead of
// `git show <merge base>:<path>` it reads /base/<path>, a read-only export
// of the merge base (the deployed code on the target branch) taken from the
// review checkout (core.TreeExporter: regular files 0444, no .git, no
// .claude/ or .mcp.json), capped by review.context.max_repo_mb: with the
// checkout cache (review.checkout_cache) the shared cached tree of the merge
// base, kept for the cache TTL; otherwise a `git archive` export removed
// when the run is over. Docker mode mounts it read-only at /base;
// local mode passes the host path. Only the merge base is exported: every
// prompt compares with it (the incremental range of a re-review is in the
// prompt's diff; the whole change of the merge request is /base vs the
// working tree).
//
// The bot's own checks (migrations evidence, compat) keep reading the
// merge base through the checkout (ShowFile, GrepWords), not through this
// export.

import (
	"context"
	"errors"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Base is the merge base export of one run. A zero value (no export) is
// valid: Dir and Root are empty and Release does nothing.
type Base struct {
	Dir  string // host path ("" = none), core.RunRequest.BaseDir
	Root string // the path Claude sees ("/base" in docker mode), for prompts
	rel  func()
}

// PrepareBase exports the merge base of mr from co. It never fails the
// caller: without an export the prompts fall back to the diff only and a
// problem is recorded.
func (e *Env) PrepareBase(ctx context.Context, job core.Job, co core.Checkout, mr *core.MergeRequest) *Base {
	ctx, tsp := tracing.Start(ctx, "review.base")
	defer tsp.End()
	b := &Base{}
	ex, ok := core.CheckoutAs[core.TreeExporter](co)
	sha := diffcap.FullDiffBase(mr)
	if !ok || sha == "" {
		return b
	}
	dir, release, err := ex.ExportTree(ctx, sha, e.Cfg.Review.Context.MaxRepoBytes())
	if err != nil {
		if ctx.Err() != nil {
			return b
		}
		e.Log.Warn("review: merge base export failed, Claude gets no /base", "mr", job.MRKey(), "sha", sha, "err", err)
		subject := "выгрузка merge base не удалась"
		if errors.Is(err, core.ErrTreeTooLarge) {
			subject = "репозиторий больше лимита размера"
		}
		jobctx.BotProblem(ctx, problems.BaseUnavailable, subject, err.Error())
		return b
	}
	b.Dir, b.rel = dir, release
	b.Root = core.BaseMountPath(dir, e.DockerSandbox())
	return b
}

// Release removes the export. Safe on nil and more than once.
func (b *Base) Release() {
	if b != nil && b.rel != nil {
		b.rel()
		b.rel = nil
	}
}

// HostDir is the export for core.RunRequest.BaseDir ("" on nil).
func (b *Base) HostDir() string {
	if b == nil {
		return ""
	}
	return b.Dir
}

// MountRoot is the path Claude sees ("" on nil).
func (b *Base) MountRoot() string {
	if b == nil {
		return ""
	}
	return b.Root
}
