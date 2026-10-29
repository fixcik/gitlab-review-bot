package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/labels"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// blockingOpen counts the bot threads that block the merge after the
// review (the gate line), plus the blocking results whose thread could not
// be published (fail closed).
func (rv *reviewRun) blockingOpen(t *reviewTally) int {
	n := t.newBlocking + len(rv.threads.OpenCompat) + t.fixedPending + rv.threads.OpenOtherBlocking
	n += t.size.open - t.size.pending // the pending one is in fixedPending
	n += rv.unpublished(t)
	if rv.lr != nil && !rv.lr.ScanFailed {
		n += rv.lr.BlockingOpen()
	} else {
		// Lint off now (or its thread scan failed): earlier open blocking
		// lint threads still block the merge.
		n += rv.threads.OpenLintBlocking
	}
	if rv.cr != nil {
		n += len(rv.cr.Published)
	}
	for _, f := range rv.threads.Open {
		if f.Blocking {
			n++
		}
	}
	return n
}

// unpublished counts the blocking results of this review whose thread
// could not be published: findings, breaking changes, linter results. They
// block the merge in the gate line, and the reviewed head is not advanced
// (flow step 14), so the next review publishes them.
func (rv *reviewRun) unpublished(t *reviewTally) int {
	return t.unpublished + t.size.unpublished + rv.cr.Unpublished() + rv.lr.UnpublishedBlocking()
}

// unpublishedLine is the summary warning about n blocking threads that
// could not be published.
func unpublishedLine(n int, mention string) string {
	return fmt.Sprintf("⚠️ **Не удалось опубликовать %d %s** (ошибка GitLab) — мерж всё равно считается заблокированным. "+
		"Диапазон не отмечен проверенным: следующее ревью (новый пуш или `%s review`) опубликует их снова.",
		n, mdtext.Plural(n, "блокирующий тред", "блокирующих треда", "блокирующих тредов"), mention)
}

// postSummary renders and writes the summary note, keeps it fresh while the
// head pipeline runs and reacts to a clean review. It returns the note's id
// for the state (a failed write is only logged).
func (r *Reviewer) postSummary(ctx context.Context, rv *reviewRun, t *reviewTally) int64 {
	job, mr, eff, log := rv.job, rv.mr, rv.eff, rv.log
	blockingOpen := rv.blockingOpen(t)
	sd := summary.Data{
		HeadSHA: rv.head, From: rv.from, To: rv.head, Incremental: rv.incremental, Output: rv.out,
		Published: t.published, Outside: t.outside, Skipped: t.skipped, Closed: t.closed, Mention: r.env.Mention(),
		Gate: true, BlockingOpen: blockingOpen, FixedPending: t.fixedPending, Route: rv.rt.footer(),
	}
	if c := rv.capped; c.Partial() {
		sd.Partial = &summary.Partial{Reviewed: c.Reviewed, Total: c.Total, Omitted: c.Omitted, Truncated: c.Truncated}
	}
	sd.TitleHint, sd.TaskSection = r.task.SummaryParts(mr, rv.tc, rv.out, rv.threads.SummaryBody)
	sd.TaskSection += learning.RulesSummaryLine(rv.rules) + rv.vr.SummaryBlock() // verify/apply.go
	sd.TaskSection += pathSummaryLine(rv.paths) + rv.sg.summaryLine(r.env.Mention()) + r.learning.AfterReviewOutcomes(ctx, job, mr, eff, rv.co)
	sd.TaskSection += r.learning.FinishReviewMemory(ctx, job, eff, rv.mem, rv.head)
	sd.TaskSection += rv.rc.MissingSection() // context.related that could not be provided
	sd.TaskSection = crossrepo.DiagramSection(log, eff, rv.out, rv.threads.SummaryBody, rv.incremental) + sd.TaskSection
	// Trunk-based status (pipeline/tbd.go): size, age, behind target, pipeline.
	ts := r.pipeline.CollectTBD(ctx, job, mr, eff)
	// Automatic MR labels (review/labels; never fail the review): before the
	// problems block, which shows what could not be set.
	sd.TaskSection += r.labels.Apply(ctx, job, mr, eff, rv.lp, labels.Run{Checkout: rv.co, Files: rv.files, Incremental: rv.incremental,
		Migrations: rv.mg, Compat: rv.cr, API: rv.api, Lint: rv.lr, TBD: ts})
	// The summary in the MR's own description (descsum; never fails the
	// review); the note keeps whether the block is there.
	sd.TaskSection += r.descsum.Apply(ctx, job, eff, rv.lang, rv.out, rv.threads.SummaryBody)
	// What this run could not check (problems.go): persisted now.
	pr := r.problemsSummary(ctx, eff)
	var blocks []string
	if n := rv.unpublished(t); n > 0 {
		blocks = append(blocks, unpublishedLine(n, r.env.Mention()))
	}
	sd.CompatSection = strings.Join(append(append(blocks, rv.summaryBlocks(ts, pr)...),
		linters.RenderSecrets(rv.lr)+compatcheck.RenderSection(rv.cr)+compatcheck.RenderAPISummary(rv.api)+linters.RenderSection(rv.lr)+linters.CISection(rv.lr)), "\n\n")

	body := r.withPauseLine(ctx, job, summary.Render(sd))
	summaryID, err := r.upsertSummary(ctx, job, rv.threads.SummaryID, body)
	if err != nil {
		log.Error("review: summary note", "err", err)
	} else {
		// Keep the pipeline line, coverage and CI linters fresh while it
		// runs; a finished head pipeline goes to the CI digest (pipeline/ci.go).
		r.pipeline.WatchPipeline(job, ts, rv.cov, rv.lr.WaitsForCI() || pipeline.CIRunning(eff, mr))
		r.pipeline.WatchCI(job, mr, eff, rv.threads, body)
	}
	r.problemsNotified(ctx, job, pr, err == nil) // the cc line is out (or the separate note)
	// 🎉 on the MR after a clean review (reactions.go).
	r.reactReview(ctx, job, eff, reviewClean(t.published, t.outside, blockingOpen, rv.threads, rv.lr, rv.cr, sd.Partial != nil))
	return summaryID
}

// summaryBlocks are the summary blocks above the compat section, top to
// bottom: the MR's own invalid .reviewbot.yaml (repoconfig.go), TBD, the CI
// failure block of the head pipeline (pipeline/ci.go: it survives the re-render, the
// pipeline may have failed before the review ended), coverage, what this
// run could not check, the system map and its phase B.
func (rv *reviewRun) summaryBlocks(ts *pipeline.TBDStatus, pr *problemsRun) []string {
	var blocks []string
	add := func(blk string) {
		if blk != "" {
			blocks = append(blocks, blk)
		}
	}
	add(rv.gate.summaryLine)
	add(pipeline.RenderTBD(ts, rv.eff.TBD))
	add(pipeline.KeptCIBlock(rv.threads.SummaryBody, rv.head))
	add(pipeline.RenderCoverage(rv.cov))
	if pr != nil {
		blocks = append(blocks, pr.block) // even an empty one
	}
	if rv.sm != nil {
		add(rv.sm.Block)
	}
	if rv.smDeps != nil {
		add(rv.smDeps.Block)
	}
	return blocks
}

// upsertSummary updates the summary note or creates it.
func (r *Reviewer) upsertSummary(ctx context.Context, job core.Job, noteID int64, body string) (int64, error) {
	ctx, tsp := tracing.Start(ctx, "review.summary", tracing.WithAttrs(tracing.Bool("reviewbot.update", noteID != 0),
		tracing.Int("reviewbot.body_bytes", len(body))))
	defer tsp.End()
	if noteID != 0 {
		err := r.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, noteID, body)
		if err == nil {
			return noteID, nil
		}
		if !errors.Is(err, core.ErrNotFound) {
			return noteID, fmt.Errorf("update summary: %w", err)
		}
	}
	id, err := r.env.GitLab.CreateNote(ctx, job.ProjectID, job.MRIID, body)
	if err != nil {
		return 0, fmt.Errorf("create summary: %w", err)
	}
	return id, nil
}

// notePostponed puts "⏳ Ревью отложено до HH:MM" into the summary note.
func (r *Reviewer) notePostponed(ctx context.Context, job core.Job, state *core.MRState, summaryID int64, at time.Time, reason string) {
	status := fmt.Sprintf(msgPostponed, r.env.Clock(at), strings.TrimSuffix(reason, "."))
	r.setSummaryStatus(ctx, job, state, summaryID, status)
}

func (r *Reviewer) noteFailed(ctx context.Context, job core.Job, state *core.MRState, summaryID int64, reason string) {
	r.setSummaryStatus(ctx, job, state, summaryID, fmt.Sprintf(msgReviewFailed, reason))
}

// setSummaryStatus inserts a status line into the existing summary note (or
// creates a minimal one) and remembers the note id.
func (r *Reviewer) setSummaryStatus(ctx context.Context, job core.Job, state *core.MRState, summaryID int64, status string) {
	log := r.env.Log.With("mr", job.MRKey())
	if summaryID == 0 && state != nil {
		summaryID = state.SummaryNoteID
	}
	existing := ""
	t, err := r.pub.Scan(ctx, job.ProjectID, job.MRIID, summaryID)
	switch {
	case err != nil:
		log.Warn("review: scan discussions for summary", "err", err)
		return
	case t.SummaryID != 0:
		summaryID, existing = t.SummaryID, t.SummaryBody
	default:
		summaryID = 0 // unknown or deleted: create a fresh note
	}
	sha := ""
	if state != nil {
		sha = state.ReviewedSHA
	}
	id, err := r.upsertSummary(ctx, job, summaryID, summary.SetStatus(existing, status, sha, r.env.Mention()))
	if err != nil {
		log.Warn("review: summary status", "err", err)
		return
	}
	if state != nil && state.SummaryNoteID == id {
		return
	}
	s := core.MRState{ProjectID: job.ProjectID, MRIID: job.MRIID, SummaryNoteID: id, UpdatedAt: r.env.Now()}
	if state != nil {
		s.ReviewedSHA = state.ReviewedSHA
	}
	if err := r.env.Store.SaveMRState(ctx, s); err != nil {
		log.Warn("review: save state", "err", err)
	}
}
