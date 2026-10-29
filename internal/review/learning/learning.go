// Package learning is the learning feature of the review bot: it learns the
// team's review rules and keeps a per-project operational memory, and hands
// both to the review, chat and verification prompts.
//
//   - Rules (learn.go, rules_cmd.go, rules_prompt.go, chat.go): a tool-less
//     light-model run extracts a rule from a Developer+ reply in a thread the
//     bot started (HandleThreadReply); the chat answer may carry one
//     (LearnFromChat); "@bot запомни / забудь / правила" manage them
//     (HandleRules). Rules are stored in learn.Store and rendered into the
//     prompts (PromptRules, RenderRulesBlock, RulesSystemNote). See
//     internal/learn for the rule model and docs/DESIGN.md ("Team rules").
//   - Outcomes (outcomes.go, outcomes_classify.go, outcomes_synth.go): what
//     happened to every finding the bot posted (fixed, dismissed, resolved by
//     the bot), classified on the next review; enough dismissed findings of a
//     project are synthesized into rules.
//   - Memory (memory.go, memory_update.go, memory_compact.go, memory_cmd.go):
//     per-project operational notes read at the start of every run and
//     updated from the optional "memory_updates" of Claude's answer.
//
// Dependencies: env.Env (GitLab, runner, limiter, usage store, config,
// replies), learn.Store, learn.OutcomeStore (optional), memory.Store, jobctx (the 🧠 reaction, token attribution). The
// orchestrator provides the award emoji of the job's note (NoteReactions,
// implemented in review/reactions.go) and env.RepoConfig.
package learning

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
)

// ExtractTimeout bounds one tool-less light-model run: rule extraction /
// normalization, memory compaction and rule synthesis.
const ExtractTimeout = 3 * time.Minute

// Feature learns team rules (learning/learn.go, rules_cmd.go, chat.go: from
// thread replies, chat and "@bot rules"; outcomes*.go: from what happened to
// the bot's findings) and keeps the per-project operational memory
// (memory*.go).
type Feature struct {
	env *env.Env
	// reacts: the award emoji of the job's note.
	reacts NoteReactions
	// learnings stores learned team rules (*store.SQLite); nil disables
	// learning.
	learnings learn.Store
	// outcomes stores finding outcomes (*store.SQLite); nil (or no
	// learnings) disables outcome tracking.
	outcomes learn.OutcomeStore
	// memory: per-project operational memory; may be nil.
	memory memory.Store
	// Scratch is where tool-less extraction runs get their empty working
	// directory (default <data_dir>/scratch). Exported for tests.
	Scratch string
	// Chat answers a reply that turned out to be a question to the bot
	// (job.Question is its text); nil = such a reply is dropped.
	Chat func(ctx context.Context, job core.Job) error
	// Dismiss closes the bot thread disc (the author of the reply, a
	// Developer or above, said the remark is not a problem), lifting its
	// blocking first; acted is false when there was nothing to do (not a
	// finding of the model, already closed). nil = the reply only teaches.
	Dismiss func(ctx context.Context, job core.Job, disc *core.Discussion) (acted bool, err error)
}

// New builds the learning feature. learnings (nil disables learning),
// outcomes (nil disables outcome tracking) and mem (nil disables the project
// memory) are usually *store.SQLite;
// scratch is the base of the tool-less runs' working directories ("" =
// <data_dir>/scratch).
func New(e *env.Env, reacts NoteReactions, learnings learn.Store, outcomes learn.OutcomeStore, mem memory.Store, scratch string) *Feature {
	return &Feature{env: e, reacts: reacts, learnings: learnings, outcomes: outcomes, memory: mem, Scratch: scratch}
}

// NoteReactions is what learning needs from the award emoji of the job's
// note (review/reactions.go).
type NoteReactions interface {
	// ReactsTo reports whether the job's note may get reactions.
	ReactsTo(job core.Job) bool
	// ThreadReply reacts to a pure thanks / acknowledgement in a bot thread;
	// true means it was one (nothing to learn).
	ThreadReply(ctx context.Context, job core.Job, disc *core.Discussion, idx int) bool
	// PickedUp starts the 👀 lifecycle of a reply that is being read.
	PickedUp(ctx context.Context)
}

// learnOn reports whether learning is configured for the service.
func (l *Feature) learnOn() bool {
	return l.learnings != nil && l.env.Cfg.Review.Learn.Enabled
}

// RulesOn reports whether rules are learned from and applied in a repo.
func (l *Feature) RulesOn(eff config.Effective) bool {
	return l.learnOn() && eff.LearnEnabled
}

// reserveLearn books one learning run (u as core.UsageLearn) under the
// global hourly limit and review.learn.max_per_hour, checked and booked in
// one step (Limiter.Reserve) so concurrent learning jobs cannot all take the
// last slot. Denied: ok false and the reason. The caller releases the
// booking when it ends (a no-op after the run committed it) and passes it to
// env.RunToolLess.
func (l *Feature) reserveLearn(ctx context.Context, u core.Usage) (booked core.Reservation, reason string, ok bool) {
	u.Kind = core.UsageLearn
	dec, booked, err := l.env.Limiter.Reserve(ctx, core.ReserveRequest{Usage: u, Global: true, KindPerHour: l.env.Cfg.Review.Learn.MaxPerHour})
	if err != nil {
		return nil, err.Error(), false
	}
	if !dec.Allowed {
		return nil, dec.Reason, false
	}
	return booked, "", true
}

// release drops a booking that was not used (nil-safe).
func release(b core.Reservation) {
	if b != nil {
		b.Release()
	}
}

// ScratchDir creates an empty directory for a tool-less Claude run. It lives
// under data_dir (not /tmp) so that docker-in-docker deployments, which mount
// data_dir at the same path on the host, can bind-mount it.
func (l *Feature) ScratchDir() (string, func(), error) {
	base := l.Scratch
	if base == "" {
		base = filepath.Join(l.env.Cfg.DataDir, "scratch")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, fmt.Errorf("scratch dir: %w", err)
	}
	dir, err := os.MkdirTemp(base, "learn-")
	if err != nil {
		return "", nil, fmt.Errorf("scratch dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}
