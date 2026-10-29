package pipeline

// Catching and explaining CI pipeline failures (review.ci,
// docs/features/ci.md). core.JobPipeline comes from the Pipeline Hook, the
// pipeline poller, or the review's own pipeline watch (pipeline/tbd.go chain); the
// handler re-reads everything it acts on and is idempotent, so duplicates
// cost a few GitLab reads.
//
// MR scope (the head pipeline of an open MR):
//   - failed: the summary gets the block `<!-- reviewbot:ci … -->` "🔴
//     Пайплайн #N упал" with the digest of the failed jobs (pipeline/ci_digest.go),
//     and ONE explanation run (pipeline/ci_explain.go) answers in ONE bot thread "🔴
//     Почему упал пайплайн" (not merge-blocking: no blocking marker, not in
//     the gate line), edited in place for newer failures. At most one run
//     per pipeline; a pipeline that fails the same way as the explained one
//     (same fingerprint) reuses the explanation.
//   - success: the block is removed from the summary (the trunk-based block
//     shows the green pipeline) and the thread becomes "✅ Пайплайн #M
//     прошёл" and is resolved; a later failure opens a new thread.
// Main branch scope: pipeline/ci_main.go.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/citelink"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// ciBlockPipeline returns the pipeline and sha of the summary's CI block
// (0 without one).
func ciBlockPipeline(body string) (int64, string) {
	id, sha, _ := ciBlockInfo(body)
	return id, sha
}

// ciBlockInfo also returns the fingerprint of the failure the block shows
// ("" for a block written before it was recorded).
func ciBlockInfo(body string) (int64, string, string) {
	blk := marker.CIBlockRe.FindString(body)
	m := marker.CIBlockHdrRe.FindStringSubmatch(blk)
	if m == nil {
		return 0, "", ""
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id, m[2], m[3]
}

// KeptCIBlock is the CI block of a previous summary when it is about the
// head being reviewed: a re-rendered summary keeps it (the pipeline may
// fail before the review finishes).
func KeptCIBlock(prev, head string) string {
	id, sha := ciBlockPipeline(prev)
	if id == 0 || head == "" || !strings.EqualFold(sha, head) {
		return ""
	}
	return marker.CIBlockRe.FindString(prev)
}

// replaceCI swaps the CI block in a summary body ("" removes it). A new
// block goes after the trunk-based block, else before the compat section,
// else before the footer.
func replaceCI(body, block string) string {
	if block == "" && marker.CIBlockRe.MatchString(body) {
		// Removed: do not leave the blank lines of both sides behind.
		return strings.Replace(summary.SwapBlock(body, "", marker.CIBlockRe), "\n\n\n\n", "\n\n", 1)
	}
	return summary.SwapBlock(body, block, marker.CIBlockRe, summary.AfterTBD, summary.BeforeCompat, summary.BeforeFooter)
}

// renderCIBlock is the summary block of a failed head pipeline. Its content
// is defused (mdtext.DefuseComments): only the bot's own start and end markers are
// comments, so replaceCI always finds the whole block.
func renderCIBlock(d *ciDigest, threaded bool, note string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### 🔴 Пайплайн %s упал\n\n", cijob.PipelineLink(d.Pipeline))
	b.WriteString(renderCIDigest(d, true))
	if threaded {
		b.WriteString("\nПочему — в треде «🔴 " + ciThreadTitle + "».\n")
	}
	if note != "" {
		b.WriteString("\nℹ️ " + note + "\n")
	}
	return marker.CIBlockStart(d.Pipeline, d.Fingerprint) + "\n" + mdtext.DefuseComments(b.String()) + marker.CIBlockEnd
}

// HandlePipeline handles core.JobPipeline. Never runs Claude except for the
// explanation (review.ci.explain); never replies.
func (pf *Feature) HandlePipeline(ctx context.Context, job core.Job) error {
	if job.Pipeline == nil {
		return nil
	}
	switch {
	case job.MRIID != 0:
		return pf.handleMRPipeline(ctx, job)
	case job.Pipeline.Main:
		return pf.handleMainPipeline(ctx, job) // pipeline/ci_main.go
	}
	return pf.routeBranchPipeline(ctx, job)
}

// routeBranchPipeline: a branch pipeline (not main, not an MR pipeline)
// belongs to the open MRs of that branch whose head it ran for.
func (pf *Feature) routeBranchPipeline(ctx context.Context, job core.Job) error {
	cm, ok := core.GitLabAs[CIMain](pf.env.GitLab)
	if !ok || job.Pipeline.SHA == "" {
		return nil
	}
	mrs, err := cm.ListCommitMRs(ctx, job.ProjectID, job.Pipeline.SHA)
	if err != nil {
		return fmt.Errorf("merge requests of commit: %w", err)
	}
	for _, m := range mrs {
		if m.State != "opened" || m.SourceBranch != job.Pipeline.Ref {
			continue
		}
		j := job
		j.MRIID = m.IID
		pr := *job.Pipeline
		j.Pipeline = &pr
		j.EnqueuedAt = pf.env.Now()
		pf.env.Log.Info("review: ci: branch pipeline routed to its MR", "mr", j.MRKey(), "pipeline", pr.ID)
		pf.host.Enqueue(j)
	}
	return nil
}

// handleMRPipeline acts on the head pipeline of an open MR.
func (pf *Feature) handleMRPipeline(ctx context.Context, job core.Job) error {
	log := pf.env.Log.With("mr", job.MRKey(), "kind", job.Kind)
	mr, err := pf.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get mr: %w", err)
	}
	if mr.State != "opened" || mr.Draft {
		return nil // drafts are not reviewed; their pipelines are work in progress
	}
	// "@bot pause" and "@bot ignore" in the description stop the automatic
	// CI digest like automatic reviews: no summary edit, no explanation, no
	// thread.
	skip, err := pf.host.SkipAuto(ctx, job, mr)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
	eff, ok, err := pf.env.RepoConfig.Auto(ctx, job.ProjectID, mr)
	if !ok {
		return err
	}
	if !eff.Enabled || !eff.CI.Enabled {
		return nil
	}
	p := headPipeline(mr)
	if p == nil {
		log.Debug("review: ci: head pipeline is not for the head commit, skipped")
		return nil
	}
	meter.Update(ctx, func(a *meter.Attribution) { a.MRTitle, a.Username = mr.Title, mr.AuthorUsername })
	t, err := pf.pub.ScanMR(ctx, job)
	if err != nil {
		return err
	}
	switch p.Status {
	case "failed":
		return pf.ciRed(ctx, job, mr, eff, t)
	case "success":
		pf.ciGreen(ctx, job, mr, t)
	}
	return nil
}

// ciRed handles a failed head pipeline.
func (pf *Feature) ciRed(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, t *mrthread.Threads) error {
	log := pf.env.Log.With("mr", job.MRKey())
	p := *mr.Pipeline
	d, err := pf.buildCIDigest(ctx, job.ProjectID, p, eff)
	if err != nil {
		log.Warn("review: ci: digest", "err", err)
		return nil // recorded as a problem
	}
	if len(d.Failed) == 0 {
		// Only ignored jobs failed: nothing to show.
		log.Info("review: ci: only ignored jobs failed", "ignored", d.Ignored)
		pf.setCIBlock(ctx, job, t, "")
		return nil
	}
	// A failure is (pipeline, fingerprint): the same pipeline failing again
	// after a job retry with another failure is new, a duplicate event is
	// not.
	th := t.CI
	blockPipeline, _, blockFP := ciBlockInfo(t.SummaryBody)
	inBlock := blockPipeline == p.ID && blockFP == d.Fingerprint
	fresh := !inBlock && !th.About(p.ID, d.Fingerprint)
	activeHere := th.Active() && th.About(p.ID, d.Fingerprint)

	var expl *ciExplanation
	result, note := "", ""
	var sameAs int64
	switch {
	case activeHere && th.Explained != 0:
		// This failure is explained (or shown as the same as an explained
		// one) already: a duplicate event. One explanation per (pipeline,
		// fingerprint): the same pipeline failing differently after a retry
		// is explained again.
	case !eff.CI.Explain || len(explainableJobs(d)) == 0:
	case th.Active() && th.Explained != 0 && th.FP == d.Fingerprint:
		sameAs = th.Explained
		pf.observeCIExplain(ciExplainDuplicate)
	default:
		// A Claude usage limit does not postpone the job: a postponed
		// pipeline job would hold every later job of the MR (the queue keeps
		// per-MR order) — "@bot pause", questions, reviews — for hours. The
		// digest goes out now without the explanation (result rate_limited;
		// not a bot problem), the next failure gets one.
		expl, result, _ = pf.explainMR(ctx, job, mr, eff, d)
		note = ciExplainNote(result)
	}
	if fresh {
		pf.observeCIFailures("mr", d, expl)
	}

	threaded := expl != nil || sameAs != 0 || (th.Active() && th.Explained != 0)
	pf.setCIBlock(ctx, job, t, renderCIBlock(d, threaded, note))

	links := citelink.New(pf.env.Cfg.GitLab.URL)
	links.Add(job.ProjectPath, mr.DiffRefs.HeadSHA)
	switch {
	case expl != nil:
		st := ciThreadState{Pipeline: p, FP: d.Fingerprint, Explained: p.ID, Expl: renderCIExplanation(expl, job.ProjectPath, links)}
		if t.SummaryID == 0 {
			st.Digest = renderCIDigest(d, true)
		}
		pf.upsertCIThread(ctx, job, th, renderCIThread(st))
	case sameAs != 0:
		if body, ok := ciSameFailure(th.Body, p, d.Fingerprint, sameAs); ok {
			pf.updateCIThread(ctx, job, th, body)
		}
	case t.SummaryID == 0 && !activeHere:
		// No summary to carry the digest (no review yet): the thread does.
		pf.upsertCIThread(ctx, job, th, renderCIThread(ciThreadState{Pipeline: p, FP: d.Fingerprint,
			Digest: renderCIDigest(d, true), Note: note}))
	}
	log.Info("review: ci: failed pipeline handled", "pipeline", p.ID, "failed", len(d.Failed), "explain", result, "same_as", sameAs)
	return nil
}

// ciExplainNote says why there is no explanation ("" when there is one).
func ciExplainNote(result string) string {
	switch result {
	case ciExplainOK, "":
		return ""
	case ciExplainLimit:
		return "Объяснение не построено: исчерпан общий лимит запусков Claude (`limits.global_runs_per_hour`)."
	case ciExplainRateLimit:
		return "Объяснение не построено: достигнут лимит использования Claude. Оно будет при следующем падении пайплайна."
	case ciExplainInvalid:
		return "Объяснение не построено: модель дважды ответила не по схеме (подробности в /runs)."
	}
	return "Объяснение не построено: ошибка запуска (подробности в логах бота и /runs)."
}

// explainMR checks out the MR head and runs the explanation.
func (pf *Feature) explainMR(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, d *ciDigest) (*ciExplanation, string, error) {
	co, err := pf.env.Repos.Checkout(ctx, job.ProjectPath, mr)
	if err != nil {
		pf.env.Log.Warn("review: ci: checkout", "mr", job.MRKey(), "err", err)
		pf.observeCIExplain(ciExplainError)
		return nil, ciExplainError, err
	}
	defer CloseCheckout(pf.env.Log, co)
	bs := pf.env.PrepareBase(ctx, job, co, mr)
	defer bs.Release()
	// The diff is capped (ciMaxDiffBytes) whatever its size: the
	// explanation is about the failure, not a review of the change, so
	// review.max_mr_lines does not apply.
	diff, cut := pf.ciDiff(ctx, co, diffcap.FullDiffBase(mr), eff.Ignore)
	return pf.explainCI(ctx, job, ciExplainInput{dir: co.Dir(), base: bs, digest: d, diff: diff, cut: cut,
		what: "merge request", title: mr.Title, lang: prompts.LanguageName(eff.Language)})
}

// CloseCheckout closes a checkout (meant for defer); a failure is only logged.
func CloseCheckout(log *slog.Logger, co core.Checkout) {
	if err := co.Close(); err != nil {
		log.Warn("review: close checkout", "err", err)
	}
}

// ciGreen: the head pipeline passed.
func (pf *Feature) ciGreen(ctx context.Context, job core.Job, mr *core.MergeRequest, t *mrthread.Threads) {
	pf.setCIBlock(ctx, job, t, "")
	th := t.CI
	if !th.Active() {
		return
	}
	p := *mr.Pipeline
	if err := pf.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, th.NoteID, mdtext.NeutralizeQuickActions(renderCIGreen(th, p))); err != nil {
		pf.env.Log.Warn("review: ci: green thread", "mr", job.MRKey(), "err", err)
		return
	}
	if err := pf.env.Resolve(ctx, job.ProjectID, job.MRIID, th.DiscussionID); err != nil {
		pf.env.Log.Info("review: ci: resolve the thread (needs the service token)", "mr", job.MRKey(), "err", err)
	}
	pf.env.Log.Info("review: ci: pipeline green again", "mr", job.MRKey(), "pipeline", p.ID)
}

// setCIBlock writes the CI block into the existing summary ("" removes it).
func (pf *Feature) setCIBlock(ctx context.Context, job core.Job, t *mrthread.Threads, block string) {
	if t.SummaryID == 0 {
		return
	}
	body := mdtext.NeutralizeQuickActions(replaceCI(t.SummaryBody, block))
	if body == t.SummaryBody {
		return
	}
	if err := pf.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, t.SummaryID, body); err != nil {
		pf.env.Log.Warn("review: ci: update summary", "mr", job.MRKey(), "err", err)
		return
	}
	t.SummaryBody = body
}

// CIRunning reports whether the CI digest is on and the head pipeline still
// runs (the JobRefreshStatus chain of pipeline/tbd.go keeps polling it).
func CIRunning(eff config.Effective, mr *core.MergeRequest) bool {
	return eff.CI.Enabled && headPipeline(mr) != nil && cijob.Running(mr.Pipeline)
}

// headPipeline is the MR's pipeline when it ran for the head commit.
func headPipeline(mr *core.MergeRequest) *core.Pipeline {
	p := mr.Pipeline
	if p == nil || p.SHA == "" || !strings.EqualFold(p.SHA, mr.DiffRefs.HeadSHA) {
		return nil
	}
	return p
}

// WatchCI enqueues a JobPipeline when the head pipeline has finished and
// the summary does not show it yet (failed), or a red thread waits for it
// (success). summary is the summary body as written.
func (pf *Feature) WatchCI(job core.Job, mr *core.MergeRequest, eff config.Effective, t *mrthread.Threads, summary string) {
	p := headPipeline(mr)
	if !eff.CI.Enabled || p == nil {
		return
	}
	shown, _ := ciBlockPipeline(summary)
	switch {
	case p.Status == "failed" && shown != p.ID:
	case p.Status == "success" && (shown != 0 || (t != nil && t.CI.Active())):
	default:
		return
	}
	pf.host.Enqueue(core.Job{Kind: core.JobPipeline, ProjectID: job.ProjectID, ProjectPath: job.ProjectPath, MRIID: job.MRIID,
		HeadSHA: p.SHA, EnqueuedAt: pf.env.Now(),
		Pipeline: &core.PipelineRef{ID: p.ID, Ref: mr.SourceBranch, SHA: p.SHA, Status: p.Status}})
}
