package review

import (
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/labels"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mcp"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

// Internals are the parts New wired together: the shared env and publisher
// and one instance per feature.
//
// TEST SUPPORT ONLY. The feature packages' tests (through review/reviewtest)
// build a whole Reviewer, drive it with Handle and then call or inspect one
// feature of that same reviewer (the TBD timer state, a verify step on the
// reviewer's env). Production code wires collaborators through Deps and
// must not use Internals; collaborators are set through Deps, never by
// assigning feature fields.
type Internals struct {
	Env        *env.Env
	Pub        *publish.Publisher
	Task       *task.Feature
	Lint       *linters.Feature
	Learning   *learning.Feature
	Verify     *verify.Feature
	Compat     *compatcheck.Feature
	Migrations *migcheck.Feature
	Crossrepo  *crossrepo.Feature
	Labels     *labels.Feature
	MCP        *mcp.Feature
	Pipeline   *pipeline.Feature
}

// Internals returns the reviewer's wiring (test support only, see
// Internals).
func (r *Reviewer) Internals() Internals {
	return Internals{Env: r.env, Pub: r.pub, Task: r.task, Lint: r.lint, Learning: r.learning, Verify: r.verify, Compat: r.compat, Migrations: r.migrations,
		Crossrepo: r.crossrepo, Labels: r.labels, MCP: r.mcp, Pipeline: r.pipeline}
}
