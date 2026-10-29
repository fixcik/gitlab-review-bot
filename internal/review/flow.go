package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/labels"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mcp"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

// The review job: handleReview runs its numbered steps as phases over
// reviewRun. The prompt and the answer are in review_prompt.go, publishing
// threads in review_publish.go, the summary note in review_summary.go and the
// scan of the MR's bot threads in review/publish (Scan).

// reviewRun is the state of one review job: handleReview fills it phase by
// phase, and every phase reads what the earlier ones set.
type reviewRun struct {
	job    core.Job
	log    *slog.Logger
	manual bool

	// Preflight (steps 1-4).
	mr      *core.MergeRequest
	gate    cfgGate
	eff     config.Effective
	lang    string
	resumed bool // an earlier attempt was admitted and charged (core.RunAdmitted): postponed or replayed

	// The range (steps 5-7).
	state        *core.MRState // nil: never reviewed
	knownSummary int64
	head, from   string
	incremental  bool
	co           core.Checkout
	files        []string
	diff         string
	sg           sizeGate
	capped       diffcap.Result // the diff Claude gets

	// Before the Claude run (steps 8-9).
	threads *mrthread.Threads
	*analyzers
	rt reviewRoute
	ms *mcp.Run // nil without MCP

	// The Claude run (step 10): prompt inputs, the answer and its
	// verification.
	rules []learn.Rule
	paths []matchedPath
	lp    *labels.Prompt
	mem   *learning.MemoryRun
	sk    *skillsRun
	out   *output.ReviewOutput
	vr    *verify.Run
}

// reviewTally is what the review published and closed (steps 11-12).
type reviewTally struct {
	published, outside   []output.Finding
	skipped, newBlocking int
	closed               int
	// fixedPending: blocking threads declared fixed but left unresolved
	// (Reporter bot without a service token). GitLab still counts them.
	fixedPending int
	// unpublished: blocking findings whose thread could not be published
	// (they are in outside); they still block the merge.
	unpublished int
	// size: the size thread's part of the gate (bigmr_thread.go).
	size sizeCount
}

// handleReview runs an automatic review or a review command. Its numbered
// steps are referred to across the package ("step 3", "flow step 7a").
func (r *Reviewer) handleReview(ctx context.Context, job core.Job) error {
	rv := &reviewRun{job: job, log: r.env.Log.With("mr", job.MRKey(), "kind", job.Kind), manual: job.Kind.Manual()}

	// 1-4. The MR, its repo config and the command author's permissions.
	if done, err := r.reviewPreflight(ctx, rv); done {
		return err
	}
	// 5. Range (first part: nothing new?).
	if done, err := r.reviewHeadState(ctx, rv); done {
		return err
	}

	// 6. Checkout and diff (before the limits: the size gate needs the diff).
	co, err := r.env.Repos.Checkout(ctx, job.ProjectPath, rv.mr)
	if err != nil {
		return r.failManual(ctx, job, fmt.Errorf("checkout: %w", err))
	}
	defer pipeline.CloseCheckout(rv.log, co)
	rv.co = co
	if done, err := r.reviewRange(ctx, rv); done {
		return err
	}
	// 6a. Trivial increment (trivial.go): an automatic incremental review of
	// commits that change only whitespace, comments or docs runs no Claude.
	if done, err := r.trivialIncrement(ctx, job, rv.incremental, rv.trivialRun()); done {
		return err
	}
	// 7. Size limit (bigmr.go): over review.max_mr_lines no Claude run at all,
	// only the deterministic checks and a summary note.
	rv.sg = r.checkMRSize(ctx, job, rv.mr, rv.eff, rv.diff, rv.incremental)
	if rv.sg.skip() {
		return r.reviewTooBig(ctx, job, rv.tooBigRun())
	}

	// 7a. Global limit: the run is booked together with the per-user limits,
	// so concurrent jobs cannot both take the last slot. Booked after the
	// size gate (a too-big MR runs no Claude, so it is never postponed or
	// denied by the limits) and before any Claude run. The first Claude
	// run of this job commits the booking (env.RunStructured); a job that ends
	// without one releases it.
	dec, booked, err := r.env.Limiter.Reserve(ctx, rv.reserveRequest())
	if err != nil {
		return err
	}
	if booked != nil {
		defer booked.Release()
		ctx = core.WithReservation(ctx, booked)
	}
	if !dec.Allowed {
		return r.reviewDenied(ctx, rv, dec)
	}
	rv.capped = diffcap.Cap(rv.diff, rv.eff.MaxFiles, r.env.Cfg.Review.MaxDiffBytes)
	if c := rv.capped; c.Partial() {
		jobctx.BotProblem(ctx, problems.DiffTruncated, "дифф больше лимитов: ревью частичное", fmt.Sprintf("просмотрено файлов: %d из %d, обрезан: %v", c.Reviewed, c.Total, c.Truncated))
	}

	// 8. Open bot findings.
	if rv.threads, err = r.pub.Scan(ctx, job.ProjectID, job.MRIID, rv.knownSummary); err != nil {
		return r.failManual(ctx, job, err)
	}

	// 9. Analyzers (analyzers.go; none fails the review): the merge base
	// export (/base), compat / API changelog, linters, Jira, the reviewer
	// assignment, context repositories, coverage, the system map (A and B)
	// and the migrations analysis — concurrently with
	// review.parallel_analyzers.
	rv.analyzers = r.runAnalyzers(ctx, job, analyzerIn{co: co, mr: rv.mr, eff: rv.eff, from: rv.from, diff: rv.diff,
		incremental: rv.incremental, threads: rv.threads})
	defer rv.analyzers.release()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Model routing by risk (route.go): a low-risk range goes to the light
	// model without the advisor. Decided on the analyzers' facts above.
	rv.rt = r.routeReview(ctx, job, rv.eff, rv.routeInput())
	// MCP tools of this run (package review/mcp): read tools over the results above and
	// in-loop finding validation. nil when review.mcp is off: then every ms.*
	// prompt helper returns exactly today's section. The light route runs
	// without MCP: no session overhead for a small low-risk range.
	if !rv.rt.light() {
		rv.ms = r.mcp.Open(ctx, job, rv.mr, rv.eff, rv.mcpInputs())
	}
	defer rv.ms.Close()

	// 10. Claude, with the MCP fallback: one run without the tools when the
	// CLI did not connect (mcp/fallback.go).
	ctx = r.preparePrompt(ctx, rv)
	defer rv.sk.release()
	err = r.mcp.RunReview(ctx, job, rv.ms, func(ms *mcp.Run) (core.RunRequest, func(string) error, error) {
		return r.reviewRequest(ctx, rv, ms)
	})
	rv.ms.Close() // the run is over: its MCP token is refused from now on
	if err != nil {
		return r.reviewRunFailed(ctx, rv, err)
	}
	r.settleFindings(ctx, rv)

	// 11. Publish findings, then breaking changes (always blocking) and
	// linter results.
	t := r.publishReview(ctx, rv)
	// 12. Resolved findings.
	r.closeFixed(ctx, rv, &t)
	// 13. Summary.
	summaryID := r.postSummary(ctx, rv, &t)

	// 14. State. A blocking result that could not be published keeps the
	// reviewed head where it was (fail closed): the next review covers this
	// range again and publishes it. Nothing re-runs the review by itself, so
	// this costs no extra Claude run until the next push or command.
	reviewed := rv.head
	if n := rv.unpublished(&t); n > 0 {
		reviewed = rv.reviewedBefore()
		rv.log.Warn("review: blocking results not published, reviewed head kept", "unpublished", n, "reviewed", reviewed)
	}
	if err := r.saveState(ctx, job, reviewed, summaryID); err != nil {
		return err
	}
	found := len(t.published) + len(t.outside)
	rv.log.Info("review: done", "from", rv.from, "to", rv.head, "findings", found, "closed", t.closed, "skipped", t.skipped)
	if rv.manual {
		if err := r.env.Reply(ctx, job, fmt.Sprintf(msgReviewDone, mdtext.ShortSHA(rv.from), mdtext.ShortSHA(rv.head), found, t.closed)); err != nil {
			rv.log.Warn("review: reply done", "err", err)
		}
	}
	return nil
}

// reviewPreflight re-reads the MR and decides whether it is reviewed at
// all. done: the job ends here with err.
func (r *Reviewer) reviewPreflight(ctx context.Context, rv *reviewRun) (done bool, err error) {
	job, log, manual := rv.job, rv.log, rv.manual

	// 1. Re-read the MR.
	mr, err := r.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return true, fmt.Errorf("get mr: %w", err)
	}
	rv.mr = mr
	jobctx.AttributeMR(ctx, job, mr) // auto review -> charged to the MR author
	if mr.State != "opened" {
		log.Info("review: mr not opened, skipping", "state", mr.State)
		if manual {
			return true, r.env.Reply(ctx, job, msgNotOpened)
		}
		return true, nil
	}
	// 2. Drafts.
	if mr.Draft {
		if manual {
			return true, r.env.Reply(ctx, job, msgDraft)
		}
		log.Info("review: draft mr, skipping")
		return true, nil
	}

	// 3. Repo config: opt-in, validation, the config note (repoconfig.go).
	gate, proceed, err := r.reviewConfigGate(ctx, job, mr)
	if err != nil || !proceed {
		return true, err
	}
	rv.gate, rv.eff, rv.lang = gate, gate.eff, prompts.LanguageName(gate.eff.Language)
	if !rv.eff.Enabled {
		if manual {
			return true, r.env.Reply(ctx, job, msgDisabled)
		}
		log.Info("review: disabled for repo")
		return true, nil
	}
	if !manual {
		if !rv.eff.AutoReview {
			log.Info("review: auto review disabled for repo")
			return true, nil
		}
		// "@bot pause" / ignore marker in the description (commands.go).
		skip, err := r.skipAuto(ctx, job, mr)
		return err != nil || skip, err
	}

	// 4. Permissions (the per-user limits are checked and booked together
	// with the global limit in step 7a, after the size gate).
	if rv.resumed = core.RunAdmitted(ctx); rv.resumed {
		log.Info("review: resuming postponed command")
		return false, nil
	}
	ok, err := r.env.CheckAccess(ctx, job)
	return err != nil || !ok, err
}

// fullReview reports whether the range starts at the merge base: asked for,
// or nothing was reviewed before.
func (rv *reviewRun) fullReview() bool {
	k := rv.job.Kind
	return k == core.JobFullReview || k == core.JobForceReview || rv.job.ForceFull || rv.state == nil || rv.state.ReviewedSHA == ""
}

// reviewHeadState loads the review state of the MR. done: the head is
// already reviewed and nothing asks for a full review.
func (r *Reviewer) reviewHeadState(ctx context.Context, rv *reviewRun) (done bool, err error) {
	rv.head = rv.mr.DiffRefs.HeadSHA
	if rv.state, err = r.env.Store.GetMRState(ctx, rv.job.ProjectID, rv.job.MRIID); err != nil {
		return true, fmt.Errorf("get mr state: %w", err)
	}
	if rv.state != nil {
		rv.knownSummary = rv.state.SummaryNoteID
	}
	if rv.fullReview() || rv.state.ReviewedSHA != rv.head {
		return false, nil
	}
	if rv.manual {
		return true, r.env.Reply(ctx, rv.job, fmt.Sprintf(msgNoNewCommits, mdtext.ShortSHA(rv.head), r.env.Mention()))
	}
	rv.log.Info("review: head already reviewed")
	return true, nil
}

// reviewRange decides where the range starts (the reviewed head when it is
// an ancestor of the new one, else the merge base) and reads its files and
// diff. done: there is nothing to review in the range.
func (r *Reviewer) reviewRange(ctx context.Context, rv *reviewRun) (done bool, err error) {
	job, log := rv.job, rv.log
	rv.from = diffcap.FullDiffBase(rv.mr)
	if !rv.fullReview() {
		anc, aerr := rv.co.IsAncestor(ctx, rv.state.ReviewedSHA)
		switch {
		case aerr != nil:
			log.Warn("review: ancestor check failed, doing full review", "err", aerr)
		case anc:
			rv.from, rv.incremental = rv.state.ReviewedSHA, true
		default:
			log.Info("review: previous head is not an ancestor (force push?), full review", "reviewed", rv.state.ReviewedSHA)
		}
	}
	if rv.files, err = rv.co.ChangedFiles(ctx, rv.from, rv.eff.Ignore); err != nil {
		return true, r.failManual(ctx, job, fmt.Errorf("changed files: %w", err))
	}
	if rv.diff, err = rv.co.Diff(ctx, rv.from, rv.eff.Ignore); err != nil {
		return true, r.failManual(ctx, job, fmt.Errorf("diff: %w", err))
	}
	if len(rv.files) > 0 && strings.TrimSpace(rv.diff) != "" {
		return false, nil
	}
	log.Info("review: nothing to review in range", "from", rv.from, "to", rv.head)
	r.closeSizeThreadNoChanges(ctx, job, rv.knownSummary) // bigmr.go
	// The state first: a reply that cannot be posted must not fail the job
	// (the head is reviewed, there is nothing to do again).
	if err := r.saveState(ctx, job, rv.head, rv.knownSummary); err != nil {
		return true, err
	}
	if rv.manual {
		if err := r.env.Reply(ctx, job, fmt.Sprintf(msgNothingToReview, mdtext.ShortSHA(rv.from), mdtext.ShortSHA(rv.head))); err != nil {
			log.Warn("review: reply nothing to review", "err", err)
		}
	}
	return true, nil
}

// reviewedBefore is the reviewed head before this run ("" = none).
func (rv *reviewRun) reviewedBefore() string {
	if rv.state == nil {
		return ""
	}
	return rv.state.ReviewedSHA
}

func (rv *reviewRun) trivialRun() trivialRun {
	return trivialRun{mr: rv.mr, eff: rv.eff, co: rv.co, knownSummary: rv.knownSummary, from: rv.from, head: rv.head, diff: rv.diff}
}

func (rv *reviewRun) tooBigRun() tooBigRun {
	return tooBigRun{mr: rv.mr, eff: rv.eff, co: rv.co, state: rv.state, knownSummary: rv.knownSummary,
		from: rv.from, head: rv.head, incremental: rv.incremental, files: rv.files, diff: rv.diff, gate: rv.sg}
}

// reserveRequest books the run under the global limit and, for a command
// (unless it resumes a postponed one), its author's limits.
func (rv *reviewRun) reserveRequest() core.ReserveRequest {
	req := core.ReserveRequest{Usage: env.JobUsage(rv.job), Username: rv.job.Username, Global: true}
	if rv.manual && !rv.resumed {
		ul := rv.eff.UserLimits()
		req.User = &ul
	}
	return req
}

// reviewDenied ends a review the limits did not allow: a command gets the
// reason, an automatic review is postponed.
func (r *Reviewer) reviewDenied(ctx context.Context, rv *reviewRun, dec core.Decision) error {
	if rv.manual {
		jobctx.Denied(ctx)
		return r.env.Reply(ctx, rv.job, "⛔ "+dec.Reason)
	}
	at := r.env.RetryAt(dec.RetryAt)
	r.notePostponed(ctx, rv.job, rv.state, rv.knownSummary, at, dec.Reason)
	return &core.RetryLater{At: at, Reason: dec.Reason}
}

func (rv *reviewRun) routeInput() routeInput {
	return routeInput{co: rv.co, mr: rv.mr, files: rv.files, diff: rv.diff, incremental: rv.incremental,
		smDiff: rv.smDiff, mg: rv.mg, api: rv.api, cr: rv.cr, lr: rv.lr, sm: rv.sm, smDeps: rv.smDeps}
}

func (rv *reviewRun) mcpInputs() mcp.Inputs {
	return mcp.Inputs{Checkout: rv.co, Files: rv.files, API: rv.api, CompatOn: rv.cr != nil, Lint: rv.lr, Coverage: rv.cov,
		Sysmap: rv.sm, SysmapDeps: rv.smDeps, Migrations: rv.mg, Task: rv.tc, Repos: rv.rc, Threads: rv.threads, Lang: rv.lang, Base: rv.bs.Root}
}

// reviewRunFailed ends a review whose Claude run failed: a rate limit
// postpones it, an answer off the schema fails it in the summary.
func (r *Reviewer) reviewRunFailed(ctx context.Context, rv *reviewRun, err error) error {
	job, log := rv.job, rv.log
	var rl *core.ErrClaudeRateLimited
	var inv *env.InvalidOutputError
	switch {
	case errors.As(err, &rl):
		at := r.env.RetryAt(rl.ResetAt)
		log.Warn("review: claude rate limited", "retry_at", at, "msg", rl.Message)
		r.notePostponed(ctx, job, rv.state, rv.threads.SummaryID, at, reasonClaudeLimit)
		if rv.manual {
			if rerr := r.env.Reply(ctx, job, fmt.Sprintf(msgPostponed, r.env.Clock(at), reasonClaudeLimit)); rerr != nil {
				log.Warn("review: reply postponed", "err", rerr)
			}
		}
		return &core.RetryLater{At: at, Reason: rl.Error()}
	case errors.As(err, &inv):
		jobctx.BotProblem(ctx, problems.InvalidOutput, "ответ ревью не по схеме", inv.Error())
		r.noteFailed(ctx, job, rv.state, rv.threads.SummaryID, reasonInvalidJSON)
		if rv.manual {
			if rerr := r.env.Reply(ctx, job, fmt.Sprintf(msgReviewFailed, reasonInvalidJSON)); rerr != nil {
				log.Warn("review: reply failed", "err", rerr)
			}
		}
		return err
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		return r.failManual(ctx, job, fmt.Errorf("claude run: %w", err))
	}
}

// settleFindings applies the bot's own rules to the answer's findings before
// they are published.
func (r *Reviewer) settleFindings(ctx context.Context, rv *reviewRun) {
	// Only explicit breakage of the deployed code, confirmed by Claude's
	// deployed_usage, blocks on migration files (migcheck/evidence.go): before the
	// verification and publishing.
	r.migrations.Enforce(ctx, rv.log, rv.co, rv.out, rv.mg, rv.lang)

	// Findings that repeat a linter result are never published (the linter
	// thread is): dropped before the verification so it does not spend
	// effort on them.
	rv.out.Findings = linters.DropDuplicates(rv.out.Findings, rv.lr, r.lint)

	// ONE verification run before publishing (verify/verify.go): the new
	// critical/major findings (review.verify) and Claude's new breaking
	// changes; rejected findings are dropped, downgraded ones get the
	// lower severity.
	if rv.cr != nil {
		rv.cr.CtxDirs = rv.rc.Dirs
	}
	rv.vr = r.verify.BeforePublish(ctx, rv.job, rv.eff, rv.cr, rv.out, rv.threads, rv.ms.VerifyTools(), verify.Input{
		Dir: rv.co.Dir(), Base: diffcap.FullDiffBase(rv.mr), From: rv.from, Head: rv.head, Lang: rv.lang, Diff: rv.capped.Diff, CtxDirs: rv.rc.Dirs,
		BaseDir: rv.bs.Dir, BaseRoot: rv.bs.Root, Migrations: rv.mg, Lint: rv.lr,
	})
}

// publishReview publishes the findings, then the breaking changes (always
// blocking) and the linter results.
func (r *Reviewer) publishReview(ctx context.Context, rv *reviewRun) reviewTally {
	job, mr, eff := rv.job, rv.mr, rv.eff
	idx := r.pub.Index(ctx, job, mr, len(rv.out.Findings) > 0 || (rv.cr != nil && len(rv.cr.Items) > 0) || rv.lr.NeedIndex())
	t := r.publishFindings(ctx, job, mr, idx, rv.out.Findings, rv.threads.Open)
	r.compat.Publish(ctx, job, mr, idx, rv.cr, rv.threads)
	r.learning.RecordPublished(ctx, job, mr, eff, t.published) // learning/outcomes.go
	r.verify.RecordRejected(ctx, job, mr, eff, rv.vr)          // verify/apply.go
	r.lint.Publish(ctx, job, mr, idx, rv.lr)
	return t
}

// closeFixed answers and resolves the bot threads the review found fixed.
// The model's "resolved" lists are applied only where the run could see the
// fix (resolvable): a finding in a file of the reviewed range, and never a
// blocking one on the light route.
func (r *Reviewer) closeFixed(ctx context.Context, rv *reviewRun, t *reviewTally) {
	t.fixedPending = rv.threads.FixedPendingBlocking
	files := rv.reviewedFiles()
	for _, res := range rv.out.Resolved {
		f, ok := rv.threads.Open[res.ID]
		if !ok {
			continue
		}
		if why := rv.resolvable(files, f.Path, f.Body, f.Blocking); why != "" {
			rv.log.Info("review: resolved finding ignored", "id", res.ID, "path", f.Path, "reason", why)
			continue
		}
		done, pending := r.pub.MarkFixed(ctx, rv.job, f.DiscussionID, res.Comment)
		if !done {
			continue
		}
		delete(rv.threads.Open, res.ID)
		t.closed++
		r.learning.OutcomeFixed(ctx, rv.job, rv.eff, f.DiscussionID, res.ID)
		if pending && f.Blocking {
			t.fixedPending++
		}
	}
	t.fixedPending += r.compat.ResolveFixed(ctx, rv.job, rv.cr, rv.compatResolvable(files), rv.threads)
	t.fixedPending += r.lint.ResolveFixed(ctx, rv.job, rv.lr)
	t.size = r.closeTooBigThread(ctx, rv.job, rv.sg, rv.threads) // bigmr.go
	t.fixedPending += t.size.pending
}

func (r *Reviewer) saveState(ctx context.Context, job core.Job, head string, summaryID int64) error {
	if err := r.env.Store.SaveMRState(ctx, core.MRState{
		ProjectID: job.ProjectID, MRIID: job.MRIID, ReviewedSHA: head,
		SummaryNoteID: summaryID, UpdatedAt: r.env.Now(),
	}); err != nil {
		return fmt.Errorf("save mr state: %w", err)
	}
	return nil
}

// failManual tells a command author that the review failed, then returns err.
func (r *Reviewer) failManual(ctx context.Context, job core.Job, err error) error {
	reason := failReason(ctx, err) // also for an automatic review: /problems shows a rejected token
	if job.Kind.Manual() && ctx.Err() == nil {
		if rerr := r.env.Reply(ctx, job, fmt.Sprintf(msgReviewFailed, reason)); rerr != nil {
			r.env.Log.Warn("review: reply failure", "err", rerr)
		}
	}
	return err
}
