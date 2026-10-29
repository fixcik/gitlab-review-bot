// Package compatcheck is the backward-compatibility check of a review and
// its API changelog (trunk-based development: the old and the new version of
// a service run side by side during a rolling deploy, so every change must be
// compatible both ways).
//
//   - Deterministic: spec diff tools (internal/compat) on the MR's changed
//     OpenAPI / proto / GraphQL files and repo-defined checker commands, in
//     the sandbox without network. Their results are not re-verified.
//   - Semantic: Claude reports "compatibility.breaking" in the review run
//     (SystemRules, RenderPrompt, WithSchema, NormalizeOutput); every new
//     item then goes through the verification run (review/verify; ONE run,
//     shared with the verification of findings) that tries to refute it:
//     Candidates hands the new items over, Run.AddVerified takes the
//     verdicts back. Only confirmed items are published; rejected ones are
//     listed (collapsed) in the summary. If the verification cannot run
//     (global limit, Claude limit, error), items are published marked "не
//     перепроверено" and still block.
//   - Every breaking change is its own BLOCKING thread (whatever
//     review.block_min_severity says) with the compat marker
//     (compatcheck/threads.go: Feature.Publish); it counts in the merge gate
//     line and is resolved when a later review no longer reports it
//     (Feature.ResolveFixed). The summary section is RenderSection
//     (compatcheck/section.go).
//   - The API changelog (compatcheck/apichanges.go) comes from the same
//     deterministic run: Run.API, or Feature.RunAPISummary when the check is
//     off.
//
// The package depends on the shared review packages (review/env,
// review/publish and the leaves) only; the orchestrator calls the steps from
// the review flow.
package compatcheck

import (
	"context"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Checker runs the deterministic checks (implemented by *compat.Checker).
// review.Deps.Compat may be nil: then a compat.Checker on top of
// review.Deps.Runner is used.
type Checker interface {
	Check(ctx context.Context, req compat.Request) (*compat.Result, error)
}

// SourceClaude marks compat items found by Claude.
const SourceClaude = "claude"

// maxCompatItems caps Claude-reported breaking changes per run.
const maxCompatItems = 20

// Item is one breaking change to publish.
type Item struct {
	ID     string
	Source string // compat.Source* or SourceClaude
	Path   string
	Line   int
	Rule   string // tool rule id / custom command
	Title  string
	// Message: tool text (deterministic) or Claude's explanation.
	Message          string
	Kind             string // Claude: api | event | db | config | other
	PreviousBehavior string
	NewBehavior      string
	SafeAlternative  string
	// Unverified: Claude item that could not be re-checked; VerifyNote says
	// why (or carries the verifier's reason for confirmed items).
	Unverified bool
	VerifyNote string
}

// Rejected is a Claude item the verification refuted.
type Rejected struct {
	Item   Item
	Reason string
}

// Run is the state of the compat check during one review.
type Run struct {
	// CtxDirs are the context repositories of this run, so the verification
	// pass can check whether anything actually consumes the changed contract.
	CtxDirs []core.ContextDir

	Enabled bool
	Res     *compat.Result // nil when the deterministic check failed
	Err     error
	// Filled after Claude's run.
	Items    []Item // to publish (deterministic + confirmed/unverified)
	rejected []Rejected
	// Published / Outside come from publishing.
	Published []Item
	Outside   []Item
	// dismissed are items whose thread a human resolved earlier (override):
	// not re-posted, not blocking, listed in the summary.
	dismissed []Item
	// Remaining are open compat threads (earlier runs) still unresolved.
	Remaining []mrthread.OpenCompat
	Closed    int
}

// Feature runs the compat check and publishes its threads.
type Feature struct {
	env *env.Env
	pub *publish.Publisher
	// checker runs the deterministic checks; nil means a compat.Checker on
	// top of env.Runner.
	checker Checker
}

// New returns the compat check on the shared review env and publisher;
// checker may be nil (a compat.Checker on top of env.Runner).
func New(e *env.Env, pub *publish.Publisher, checker Checker) *Feature {
	return &Feature{env: e, pub: pub, checker: checker}
}

func (cf *Feature) compatChecker() Checker {
	if cf.checker != nil {
		return cf.checker
	}
	return &compat.Checker{Runner: cf.env.Runner, Timeout: cf.env.Cfg.Review.CompatTimeout.D(), Logger: cf.env.Log}
}

// RunChecks runs the deterministic checks against the merge base.
func (cf *Feature) RunChecks(ctx context.Context, job core.Job, co core.Checkout, mr *core.MergeRequest, cfg config.Compat, lang string) *Run {
	ctx, tsp := tracing.Start(ctx, "review.compat")
	defer tsp.End()
	cr := &Run{Enabled: true}
	res, err := cf.compatChecker().Check(ctx, compat.Request{
		Checkout: co, Base: diffcap.FullDiffBase(mr), Head: mr.DiffRefs.HeadSHA, Config: compat.FromEffective(cfg), Lang: lang,
	})
	noteCompatIssues(ctx, err, issuesOf(res)) // checks that could not run
	if err != nil {
		cf.env.Log.Warn("review: compat checks failed", "mr", job.MRKey(), "err", err)
		cr.Err = err
		return cr
	}
	cr.Res = res
	return cr
}

// DeterministicItem is the item of a deterministic checker's change.
func DeterministicItem(c compat.Change) Item {
	title := c.Message
	if c.Source == compat.SourceCustom {
		title = "проверка " + mdtext.Code(c.Rule) + " сообщила о несовместимости"
	}
	return Item{
		ID: c.ID(), Source: c.Source, Path: c.Path, Line: c.Line, Rule: c.Rule,
		Title: title, Message: c.Message,
	}
}

// ClaudeItem is the item of a breaking change Claude reported.
func ClaudeItem(b output.BreakingChange) Item {
	return Item{
		ID: compat.ID(SourceClaude, b.Path, b.Title), Source: SourceClaude, Path: b.Path, Line: b.Line,
		Kind: b.Kind, Title: b.Title, Message: b.Explanation,
		PreviousBehavior: b.PreviousBehavior, NewBehavior: b.NewBehavior, SafeAlternative: b.SafeAlternative,
	}
}

// Candidates puts every deterministic change and every Claude item already
// discussed (open or dismissed thread) into cr.Items and returns Claude's new
// items, which go to the verification run (review/verify), with
// the OpenAPI changelog as their context. Items are de-duplicated by id.
func Candidates(cr *Run, out *output.ReviewOutput, threads *mrthread.Threads) (toVerify []Item, apiCtx string) {
	if cr == nil || !cr.Enabled {
		return nil, ""
	}
	seen := map[string]bool{}
	if cr.Res != nil {
		for _, c := range cr.Res.Changes {
			it := DeterministicItem(c)
			if seen[it.ID] {
				continue
			}
			seen[it.ID] = true
			cr.Items = append(cr.Items, it)
		}
	}
	if out.Compatibility != nil {
		for _, b := range out.Compatibility.Breaking {
			it := ClaudeItem(b)
			if seen[it.ID] {
				continue
			}
			seen[it.ID] = true
			_, open := threads.OpenCompat[it.ID]
			_, dismissed := threads.DismissedCompat[it.ID]
			if open || dismissed {
				cr.Items = append(cr.Items, it) // already discussed: skipped on publish
				continue
			}
			toVerify = append(toVerify, it)
		}
	}
	if len(toVerify) > 0 && cr.Res != nil {
		apiCtx = renderAPIList(cr.Res.API, apiVerifyMaxPerSpec, apiVerifyMaxTotal)
	}
	return toVerify, apiCtx
}

// AddVerified adds the verified (or unverified) Claude items to publish and
// the rejected ones to the summary list (nil-safe).
func (cr *Run) AddVerified(items []Item, rejected []Rejected) {
	if cr == nil {
		return
	}
	cr.Items = append(cr.Items, items...)
	cr.rejected = append(cr.rejected, rejected...)
}

// noteCompatIssues: compat / API changelog checks that could not run.
func noteCompatIssues(ctx context.Context, err error, issues []compat.Issue) {
	if err != nil {
		jobctx.BotProblem(ctx, problems.CompatFailed, "проверка совместимости: внутренняя ошибка", err.Error())
	}
	for _, is := range issues {
		if is.Kind != compat.IssueUnavailable && is.Kind != compat.IssueFailed {
			continue
		}
		what := "недоступна"
		if is.Kind == compat.IssueFailed {
			what = "упала"
		}
		jobctx.BotProblem(ctx, problems.CompatFailed, is.Source+" "+is.Path+": "+what, is.Reason)
	}
}

// issuesOf is res.Issues (nil for a nil result).
func issuesOf(res *compat.Result) []compat.Issue {
	if res == nil {
		return nil
	}
	return res.Issues
}
