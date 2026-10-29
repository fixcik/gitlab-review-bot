package review

// The review's analyzers between the checkout and the Claude run: the merge
// base export, compat / API changelog, linters (gitleaks, TODO, CI linter
// reports), Jira, the reviewer assignment, context repositories, coverage,
// the system map (phases A and B) and the migrations analysis. None of them
// fails the review.
//
// With review.parallel_analyzers (default) they run concurrently
// (parallel.go) along their real dependencies:
//
//	base, compat, lint (+ CI linters, needs the open threads), task,
//	reviewer, context, coverage, sysmap diff     — independent
//	sysmap A        <- compat (API endpoints), context, sysmap diff
//	sysmap B (deps) <- sysmap A, task, context
//	migrations      <- sysmap diff (the full MR diff of an incremental review)
//
// Shared state: every step writes only its own result; the reviewer's
// caches (todoKeys, cfgCache, …) have their own locks; the GitLab memo
// (glmemo.go), the problems collector, diag runs and spans are safe for
// concurrent use; bot problems are buffered per step and added in the order
// above. The steps that write into the checkout use their own scratch
// directories (lint, compat) and safefs serializes .git/info/exclude.

import (
	"context"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
)

// analyzerIn is what the analyzers read (never written by them).
type analyzerIn struct {
	co          core.Checkout
	mr          *core.MergeRequest
	eff         config.Effective
	from, diff  string
	incremental bool
	threads     *mrthread.Threads
}

// analyzers are their results. release frees the base export and the
// context snapshots (safe on partial results).
type analyzers struct {
	bs     *env.Base
	cr     *compatcheck.Run
	api    *compatcheck.APIRun
	lr     *linters.Run
	tc     *task.Context
	rc     *crossrepo.RunContext
	cov    *pipeline.CoverageRun
	sm     *crossrepo.SysmapRun
	smDeps *crossrepo.DepsRun
	mg     *migcheck.Run
	// smDiff is the full MR diff of the system map (sysmapDiff; "" when the
	// map is off), also a fact of the model routing (route.go).
	smDiff string
}

func (a *analyzers) release() {
	a.bs.Release()
	if a.rc != nil && a.rc.Release != nil {
		a.rc.Release()
	}
}

// parallelAnalyzers reports whether the analyzers run concurrently.
func (r *Reviewer) parallelAnalyzers() bool { return r.env.Cfg.Review.ParallelAnalyzers }

// runAnalyzers runs the analyzers and waits for all of them. With ctx
// cancelled some results may be missing: the caller checks ctx.Err().
func (r *Reviewer) runAnalyzers(ctx context.Context, job core.Job, in analyzerIn) *analyzers {
	a := &analyzers{}
	defer func() {
		if p := recover(); p != nil { // a step panicked (re-raised by wait)
			a.release()
			panic(p)
		}
	}()
	co, mr, eff := in.co, in.mr, in.eff
	g := newStepGroup(ctx, r.parallelAnalyzers())

	// The code before the merge request for Claude: a read-only export of
	// the merge base (/base; env/base.go). Claude has no git.
	g.add("base", func(ctx context.Context) { a.bs = r.env.PrepareBase(ctx, job, co, mr) })

	// Backward-compatibility: deterministic spec checks. The API changelog
	// (compatcheck/apichanges.go) comes from the same run; with the check off it is
	// still built, without blocking threads.
	compat := g.add("compat", func(ctx context.Context) {
		if eff.Compat.Enabled {
			a.cr = r.compat.RunChecks(ctx, job, co, mr, eff.Compat, eff.Language)
			a.api = a.cr.API()
		} else {
			a.api = r.compat.RunAPISummary(ctx, job, co, mr, eff.Compat, eff.Language)
		}
	})

	// Linters and security scanners on the changed lines (linters/lint.go), the
	// reports of the repository's CI linters (linters/cilint.go).
	g.add("lint", func(ctx context.Context) {
		a.lr = r.lint.Run(ctx, job, co, mr, eff, in.from, in.diff)
		r.lint.AddCI(ctx, job, mr, eff, a.lr, in.threads)
		linters.NoteProblems(ctx, a.lr)
	})

	// Jira issues and the MR title.
	taskStep := g.add("task", func(ctx context.Context) {
		a.tc = r.task.BuildContext(ctx, job, mr, eff.JiraProjectKeys)
		a.tc.Title = task.TitleFormat{Prompt: eff.TitlePrompt, Language: eff.TitleLanguage}
		task.NoteProblems(ctx, a.tc)
	})

	// The bot shows up as a reviewer of the MR (service token).
	g.add("reviewer", func(ctx context.Context) { r.ensureReviewer(ctx, job) })

	// Context repositories (read-only).
	rctx := g.add("context", func(ctx context.Context) {
		a.rc = r.crossrepo.AcquireContext(ctx, job, eff, mr, false)
		a.rc.NoteProblems(ctx)
	})

	// Diff coverage from the CI artifact (pipeline/coverage.go): the uncovered
	// changed lines go into the user prompt.
	g.add("coverage", func(ctx context.Context) {
		a.cov = r.pipeline.CollectCoverage(ctx, job, mr, eff)
		pipeline.NoteCoverageProblems(ctx, a.cov)
	})

	// System map (crossrepo/sysmap.go): contracts this MR touches and their users in
	// other repositories. Phase B (crossrepo/sysmapdeps.go): what it starts or stops
	// using elsewhere, whether that is deployed, related open MRs.
	diffStep := g.add("sysmap_diff", func(ctx context.Context) {
		a.smDiff = r.crossrepo.SysmapDiff(ctx, job, co, mr, eff, in.diff, in.incremental)
	})
	sysmapA := g.add("sysmap", func(ctx context.Context) {
		a.sm = r.crossrepo.CollectSysmap(ctx, job, a.smDiff, a.api, a.rc)
	}, compat, rctx, diffStep)
	g.add("sysmap_deps", func(ctx context.Context) {
		a.smDeps = r.crossrepo.CollectSysmapDeps(ctx, job, co, mr, eff, a.smDiff, a.sm, a.tc, a.rc)
	}, sysmapA, taskStep, rctx)

	// Migration safety (migcheck/migrations.go): hints on where to look in the MR's
	// migrations.
	g.add("migrations", func(ctx context.Context) {
		a.mg = r.migrations.Collect(ctx, job, co, mr, eff, r.migrations.Diff(ctx, job, co, mr, eff, in.diff, a.smDiff, in.incremental))
	}, diffStep)

	g.wait()
	if a.mg != nil && a.bs != nil {
		a.mg.WithBaseDir(a.bs.Root)
	}
	return a
}
