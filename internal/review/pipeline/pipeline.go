// Package pipeline follows the CI pipelines of merge requests and of the
// main branches, and shows what they say in the review.
//
//   - CI failures of an MR (review.ci; pipeline/ci.go, ci_thread.go): the
//     summary gets the digest of a failed head pipeline (pipeline/ci_digest.go,
//     ci_log.go, ci_digest_render.go: GitLab data only, no Claude) and one
//     Claude run explains it in one bot thread (pipeline/ci_explain.go,
//     ci_explain_prompt.go); the thread is resolved when the pipeline is
//     green again. HandlePipeline handles core.JobPipeline.
//   - CI failures of a main branch (review.ci.main_branch;
//     pipeline/ci_main.go): one note in the merged MR that most likely broke
//     it, or a commit comment after a direct push.
//   - The trunk-based development block of the summary (review.tbd;
//     pipeline/tbd.go): size, age, how far the branch is behind, head
//     pipeline. WatchPipeline starts a core.JobRefreshStatus chain
//     (HandleRefreshStatus) that re-renders it, the coverage block and the
//     CI linter reports while the pipeline runs, and WatchCI enqueues the
//     JobPipeline once it has finished.
//   - Coverage of the changed lines from the CI coverage reports
//     (review.coverage; pipeline/coverage.go): the summary block and the
//     numbers for Claude's prompt (CoverageForClaude, the MCP coverage tool).
//
// The package depends on the shared review packages: env.Env (GitLab,
// checkouts and /base, the Claude runner and limiter, the orchestrator's
// env.RepoConfig, of which it uses Auto), publish.Publisher (ScanMR,
// Drafts), jobctx (bot problems, usage kind) and linters.Feature (the CI
// linter reports refreshed with the pipeline). From the orchestrator it
// needs Host (review/commands.go: pipelineHost). What it reads from GitLab
// are optional capabilities of the client, looked up with core.GitLabAs:
// CIFailures, CIMain, CommitComments and cijob.Artifacts; without one the
// feature that needs it does nothing.
package pipeline

import (
	"context"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
)

// Feature follows the MR's and the main branch's CI pipelines (the CI
// block and thread, the failure digest and explanation), keeps the
// trunk-based development block fresh and collects coverage.
type Feature struct {
	env  *env.Env
	pub  *publish.Publisher
	lint *linters.Feature // CI linter reports refreshed with the pipeline
	host Host
	// Refreshing: MRs whose status refresh chain is running
	// (pipeline/tbd.go). Exported for tests.
	Refreshing refreshWatch
	// scrub removes secrets from job logs and test messages; obs receives
	// the CI failure metrics (may be nil).
	scrub func(string) string
	obs   Observer
	// mainStore keeps the state of each main branch; nil = no main branch
	// scope.
	mainStore CIMainStore
}

// New builds the pipeline feature. lint refreshes the CI linter reports
// with the pipeline; host enqueues jobs. scrub removes secrets from job logs
// and test messages (required); obs (CI failure metrics) and mainStore (the
// state of each main branch; nil = no main branch scope) may be nil.
func New(e *env.Env, pub *publish.Publisher, lint *linters.Feature, host Host, scrub func(string) string, obs Observer, mainStore CIMainStore) *Feature {
	return &Feature{env: e, pub: pub, lint: lint, host: host, scrub: scrub, obs: obs, mainStore: mainStore}
}

// Host is what the CI features need from the job orchestrator
// (review/commands.go: pipelineHost).
type Host interface {
	// Enqueue schedules a job; false when no queue is wired.
	Enqueue(job core.Job) bool
	// SkipAuto reports whether automatic work on mr must be skipped: the MR
	// is paused, or its description carries the ignore marker.
	SkipAuto(ctx context.Context, job core.Job, mr *core.MergeRequest) (bool, error)
}

// Observer receives the CI failure metrics (*telemetry.Metrics).
type Observer interface {
	ObserveCIFailure(scope, cause string)
	ObserveCIExplain(result string)
}

func (pf *Feature) observeCIFailure(scope, cause string) {
	if pf.obs != nil {
		pf.obs.ObserveCIFailure(scope, cause)
	}
}

// observeCIFailures counts the failed jobs of a digest by cause (a lint.ci
// job is the change's own lint error).
func (pf *Feature) observeCIFailures(scope string, d *ciDigest, expl *ciExplanation) {
	for _, jd := range d.Failed {
		cause := expl.cause(jd.Job.Name)
		if jd.Kind == ciKindLint {
			cause = ciCauseMR
		}
		pf.observeCIFailure(scope, cause)
	}
}

func (pf *Feature) observeCIExplain(result string) {
	if pf.obs != nil {
		pf.obs.ObserveCIExplain(result)
	}
}
