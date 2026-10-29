// Package linters is the lint step of a review: deterministic results the
// bot publishes itself as threads, only on the lines the MR changes, without
// Claude.
//
//   - Linters and scanners the bot runs in the sandbox (internal/lint;
//     linters/lint.go). gitleaks: every leaked secret is a BLOCKING thread
//     (critical, whatever review.block_min_severity says); the note never
//     contains the value.
//   - CI linters (.reviewbot.yaml lint.ci; linters/cilint.go,
//     cilint_section.go, cilint_refresh.go): reports of the repository's own
//     CI jobs, tool "ci:<name>"; never blocking, unless the entry is marked
//     blocking (then review.block_min_severity decides). Reports of jobs
//     still running are read later by the pipeline feature's status refresh
//     (RefreshCI), without a new Claude run.
//   - The todo check (review.todo; linters/todo.go): TODO / FIXME / XXX /
//     HACK comments added without a task reference, tool "todo"; it runs
//     when review.lint is off too.
//
// Threads carry `<!-- reviewbot:lint id=<id> tool=<tool> -->` (NoteBody,
// linters/note.go); any thread with the id (open or resolved) suppresses a
// repost. An open thread is closed when its tool ran completely this time
// and no longer reports the id (same Moderator / Reporter path as findings:
// Publish, ResolveFixed). Results also go to Claude's prompt as data, with
// the instruction not to repeat them (RenderPrompt, linters/prompt.go);
// Claude findings that repeat one anyway are dropped (DropDuplicates,
// linters/dedupe.go). The summary gets the secrets block and the
// "🔧 Линтеры и сканеры" / "🔧 Линтеры из CI" blocks (linters/section.go,
// linters/cilint_section.go); GateCount (linters/gate.go) counts the
// merge-blocking threads for the gate line.
//
// The package depends on the shared review packages only: env.Env (GitLab,
// runner, logger, clock, Blocking, the moderator), publish.Publisher (thread
// scan, drafts and anchors, MarkFixed, ResolveNonBlocking), task.Feature
// (Jira lookups of review.todo.verify_keys) and jobctx (bot problems). It
// needs no orchestrator interface; the orchestrator and the MCP tools pass
// *Feature as the Gate of DropDuplicates / DuplicateOf. review.Deps.Lint
// (Checker) replaces the sandboxed lint.Checker, e.g. in tests.
package linters

import (
	"context"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
)

// Checker runs the tools (implemented by *lint.Checker). review.Deps.Lint
// may be nil: then a lint.Checker on top of review.Deps.Runner is used.
type Checker interface {
	Check(ctx context.Context, req lint.Request) (*lint.Result, error)
}

// Feature runs linters and scanners on the MR (linters/lint.go), reads the
// CI linter reports (linters/cilint.go) and the TODO markers
// (linters/todo.go), and keeps their threads.
type Feature struct {
	env  *env.Env
	pub  *publish.Publisher
	task *task.Feature // Jira lookups of review.todo.verify_keys
	// Checker runs the linters and scanners; nil = a lint.Checker on top of
	// env.Runner. Exported for tests.
	Checker Checker
	// todoKeys: Jira lookups of review.todo.verify_keys (linters/todo.go).
	todoKeys todoKeyCache
}

// New builds the lint feature. tk provides the Jira lookups of
// review.todo.verify_keys; checker is nil to run a lint.Checker on top of
// env.Runner.
func New(e *env.Env, pub *publish.Publisher, tk *task.Feature, checker Checker) *Feature {
	return &Feature{env: e, pub: pub, task: tk, Checker: checker}
}

// lintChecker is Checker, or a lint.Checker on top of env.Runner.
func (lf *Feature) lintChecker(cfg config.Lint) Checker {
	if lf.Checker != nil {
		return lf.Checker
	}
	return &lint.Checker{Runner: lf.env.Runner, Timeout: cfg.Timeout, Logger: lf.env.Log}
}
