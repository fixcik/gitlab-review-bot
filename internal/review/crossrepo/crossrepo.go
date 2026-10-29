// Package crossrepo is what a review knows beyond the reviewed repository.
//
//   - Context repositories (review.context; crossrepo/context.go):
//     read-only snapshots of the other repositories of gitlab.groups, the
//     related ones (context.related) first, mounted for Claude's review,
//     check and chat runs. AcquireContext returns a RunContext: the
//     directories, the prompt block, the linker that turns Claude's
//     `group/project:path:line` citations into links, and the warning about
//     related repositories that could not be provided.
//   - System map, phase A (review.sysmap; crossrepo/sysmap.go,
//     diffparse.go): which contracts of the reviewed repository the merge
//     request touches and who uses them in other repositories
//     (CollectSysmap → SysmapRun: a summary block and a system prompt
//     section).
//   - System map, phase B (review.sysmap.deps; crossrepo/sysmapdeps*.go):
//     what the merge request starts or stops consuming or provides, whether
//     the provider's default branch already has it, and the open merge
//     requests of the counterpart repositories touching the same contracts
//     (CollectSysmapDeps → DepsRun). Facts only: no thread, no gate.
//   - The Mermaid diagram of the summary (review.diagram;
//     crossrepo/diagram.go): validation of Claude's diagram, its section and
//     the prompt rules.
//
// Nothing here fails a review: without a feature, or on any error, the run
// simply gets no block.
//
// The package depends on env.Env (configuration, logger, GitLab client,
// docker sandbox), task.Feature (the Jira client and browse URLs, for the
// story a counterpart merge request shares), compatcheck.APIRun (the
// endpoints of the OpenAPI changelog) and jobctx (bot problems). From
// outside the review it takes ContextRepos (internal/contextrepos), a
// SystemMap (*sysmap.Map, which also answers SystemMapDeps) and, as an
// optional capability of the GitLab client looked up with core.GitLabAs,
// OpenMRs. It needs nothing from the orchestrator.
package crossrepo

import (
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
)

// Feature is what the review knows beyond its repository: the related
// repositories Claude may read (crossrepo/context.go), the system map of
// contracts between repositories and the counterpart merge requests
// (crossrepo/sysmap.go, sysmapdeps.go) and the diagram rules
// (crossrepo/diagram.go).
type Feature struct {
	env  *env.Env
	task *task.Feature // Jira issues of counterpart merge requests
	// ctxRepos: context repositories; nil when the feature is off.
	ctxRepos ContextRepos
	// sysmap: the system map; nil when the feature is off.
	sysmap SystemMap
}

// New builds the cross-repository feature. tk gives the Jira client and
// browse URLs; ctxRepos (context repositories) and sm (the system map) are
// nil when the respective feature is off.
func New(e *env.Env, tk *task.Feature, ctxRepos ContextRepos, sm SystemMap) *Feature {
	return &Feature{env: e, task: tk, ctxRepos: ctxRepos, sysmap: sm}
}

// SystemMap is the system map for other features (nil when off).
func (xr *Feature) SystemMap() SystemMap { return xr.sysmap }

// HasContextRepos reports whether context repositories are wired (the
// service may still have them switched off).
func (xr *Feature) HasContextRepos() bool { return xr.ctxRepos != nil }
