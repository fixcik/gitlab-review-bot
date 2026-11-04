package review

// Merge conflict gate (flow step 5a): an MR that GitLab cannot merge
// (has_conflicts) gets no Claude run. The review would read the diff against
// the merge base, which is not the code that lands in the target branch, and
// the author has to rebase anyway. The bot writes a conflict block into the
// summary note (the rest of an earlier summary stays) and leaves the reviewed
// head unchanged: the commit that resolves the conflict starts the review.
// "@bot review force" reviews the MR anyway.
//
// GitLab computes the merge status asynchronously: right after a push it is
// "checking" or "unchecked". The gate re-reads the MR a couple of times and, if
// the status is still unknown, reviews as usual: it never skips on a guess.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

const (
	// skipConflict is the OnSkip reason (reviewbot_skipped_total).
	skipConflict = "conflict"

	msgConflict = "⛔ В MR конфликт слияния с `%s`: Claude не запускался. Разрешите конфликт (rebase на `%s` или merge `%s` в ветку), ревью запустится само. Ревью всё равно: `%s review force`."
)

// mergeStatusWaits are the pauses before the re-reads of a merge status
// GitLab has not computed yet (a variable for the tests).
var mergeStatusWaits = []time.Duration{5 * time.Second, 10 * time.Second}

// mergeStatusPending: GitLab has not computed the merge status yet.
func mergeStatusPending(s string) bool {
	return s == "checking" || s == "unchecked" || s == "preparing"
}

// mergeConflicts reports whether the MR has merge conflicts. An unknown
// status (still pending after the re-reads, or an API error) is "no".
func (r *Reviewer) mergeConflicts(ctx context.Context, job core.Job, mr *core.MergeRequest) bool {
	log := r.env.Log.With("mr", job.MRKey())
	cur := mr
	for i := 0; mergeStatusPending(cur.MergeStatus); i++ {
		if i == len(mergeStatusWaits) {
			log.Info("review: merge status still unknown, reviewing", "status", cur.MergeStatus)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(mergeStatusWaits[i]):
		}
		// Fresh: the job memoizes GetMR, and the memo holds the status that is not final.
		next, err := r.env.GitLab.GetMR(jobctx.WithFreshGitLab(ctx), job.ProjectID, job.MRIID)
		if err != nil {
			log.Warn("review: re-read merge status", "err", err)
			return false
		}
		cur = next
	}
	return cur.HasConflicts
}

// reviewConflict is step 5a. done: the MR has a conflict and the summary
// note says so; Claude must not run.
func (r *Reviewer) reviewConflict(ctx context.Context, rv *reviewRun) (done bool, err error) {
	job, mr, log := rv.job, rv.mr, rv.log
	if job.Kind == core.JobForceReview || !r.mergeConflicts(ctx, job, mr) {
		return false, nil
	}
	log.Info("review: merge conflict, claude review skipped", "target", mr.TargetBranch, "head", rv.head)
	if r.onSkip != nil {
		r.onSkip(skipConflict)
	}
	threads, err := r.pub.Scan(ctx, job.ProjectID, job.MRIID, rv.knownSummary)
	if err != nil {
		return true, r.failManual(ctx, job, err)
	}
	block := mdtext.NeutralizeQuickActions(renderConflict(mr, r.env.Mention()))
	body := threads.SummaryBody
	if body == "" {
		body = marker.Summary(rv.head) + "\n" + summary.Title + "\n\n" + block + "\n"
	} else {
		body = summary.SwapBlock(body, block, marker.ConflictRe, afterTitle)
	}
	summaryID := threads.SummaryID
	if body != threads.SummaryBody {
		if summaryID, err = r.upsertSummary(ctx, job, threads.SummaryID, r.withPauseLine(ctx, job, body)); err != nil {
			log.Error("review: summary note", "err", err)
		}
	}
	// State: the summary note, the reviewed head unchanged.
	if err == nil && (rv.state == nil || rv.state.SummaryNoteID != summaryID) {
		s := core.MRState{ProjectID: job.ProjectID, MRIID: job.MRIID, SummaryNoteID: summaryID, UpdatedAt: r.env.Now()}
		if rv.state != nil {
			s.ReviewedSHA = rv.state.ReviewedSHA
		}
		if err := r.env.Store.SaveMRState(ctx, s); err != nil {
			return true, fmt.Errorf("save mr state: %w", err)
		}
	}
	if job.Kind.Manual() {
		msg := fmt.Sprintf(msgConflict, mr.TargetBranch, mr.TargetBranch, mr.TargetBranch, r.env.Mention())
		if rerr := r.env.Reply(ctx, job, msg); rerr != nil {
			log.Warn("review: reply conflict", "err", rerr)
		}
	}
	return true, nil
}

// renderConflict is the conflict block of the summary note.
func renderConflict(mr *core.MergeRequest, mention string) string {
	var b strings.Builder
	b.WriteString(marker.ConflictStart + "\n")
	fmt.Fprintf(&b, "### ⛔ Конфликт слияния с `%s`\n\n", mr.TargetBranch)
	b.WriteString("GitLab не может смержить ветку без разрешения конфликтов, поэтому Claude не запускался: ревью проверяло бы не тот код, который попадёт в целевую ветку.")
	if n := mr.DivergedCommits; n > 0 {
		fmt.Fprintf(&b, " Ветка отстаёт от `%s` на %d %s.", mr.TargetBranch, n, mdtext.Plural(n, "коммит", "коммита", "коммитов"))
	}
	fmt.Fprintf(&b, "\n\nРазрешите конфликт: rebase на `%s` или merge `%s` в ветку. Новый коммит запустит ревью сам; не дожидаясь: `%s review force`.\n", mr.TargetBranch, mr.TargetBranch, mention)
	b.WriteString(marker.ConflictEnd)
	return b.String()
}

// afterTitle puts a new summary block under the title line.
func afterTitle(body, block string) (string, bool) {
	i := strings.Index(body, summary.Title)
	if i < 0 {
		return "", false
	}
	end := strings.IndexByte(body[i:], '\n')
	if end < 0 {
		return body + "\n\n" + block, true
	}
	j := i + end + 1
	return body[:j] + "\n" + block + "\n" + body[j:], true
}
