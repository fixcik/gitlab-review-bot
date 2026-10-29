package linters

import (
	"context"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
)

// ciPendingIn returns the tools the block of a summary body still waits
// for (nil without a block).
func ciPendingIn(body string) map[string]bool {
	blk := marker.CILintRe.FindString(body)
	m := marker.CILintPendingRe.FindStringSubmatch(blk)
	if m == nil || m[1] == "" {
		return nil
	}
	out := map[string]bool{}
	for _, t := range strings.Split(m[1], ",") {
		if t != "" {
			out[t] = true
		}
	}
	return out
}

// RefreshCI re-reads the reports the summary still waits for and
// publishes / closes their lint threads, updates the merge-gate line and the
// block. No Claude run. wait reports jobs that are still running.
func (lf *Feature) RefreshCI(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, t *mrthread.Threads, body string) (out string, wait bool) {
	waited := ciPendingIn(body)
	if len(waited) == 0 || len(eff.Lint.CI) == 0 {
		return body, false
	}
	c := lf.collectCILint(ctx, job, mr, eff)
	if c == nil {
		return body, false
	}
	log := lf.env.Log.With("mr", job.MRKey(), "kind", job.Kind)
	lr := &Run{Cfg: eff.Lint, Threads: map[string]Thread{}, ci: c,
		Res: &lint.Result{Findings: c.findings, Tools: c.statuses()}}
	read := false
	for _, e := range c.entries {
		if e.st.State == lint.StateRan || e.st.State == lint.StatePartial {
			read = true
		}
		if waited[e.st.Tool] && (e.st.State == lint.StateFailed || e.st.State == lint.StateUnavailable) {
			noteLintStatus(ctx, e.st) // a problem only when the job finished without a report
		}
	}
	if read {
		lf.scanLintThreads(ctx, job, lr)
		markClaudeDuplicates(lr, t)
		idx := lf.pub.Index(ctx, job, mr, lr.NeedIndex())
		lf.Publish(ctx, job, mr, idx, lr)
		lf.ResolveFixed(ctx, job, lr) // only ci: threads: gitleaks did not run now
		if len(lr.Published) > 0 || lr.closedCI > 0 {
			nb, err := lf.RecountGate(ctx, job, t.SummaryID, body)
			if err != nil {
				log.Warn("review: ci lint: rescan discussions for the gate line", "err", err)
			}
			body = nb
		}
		log.Info("review: ci lint refreshed", "published", len(lr.Published), "closed", lr.closedCI)
	}
	return ReplaceCIBlock(body, RenderCIBlock(lr)), len(c.pending()) > 0
}
