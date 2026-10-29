package verify

// Applying the verdicts: findings rejected or downgraded, rejected ones
// recorded as outcomes, the summary block.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// applyFindingVerdicts applies the verdicts to out.Findings: rejected
// findings are removed (with every later duplicate of their id), downgraded
// ones get the lower severity and a note; anything else stays (confirm).
// Nothing changes when the run failed.
func (vf *Feature) applyFindingVerdicts(job core.Job, out *output.ReviewOutput, vr *Run, res verifyResult) {
	if vr == nil || len(vr.items) == 0 || vr.failed != "" {
		return
	}
	log := vf.env.Log.With("mr", job.MRKey())
	drop := map[string]bool{}
	for _, it := range vr.items {
		from := it.F.Severity
		fv, ok := res.verdicts[it.ID]
		if !ok {
			vr.confirmed++
			vf.observeVerdict(verdictMissing, string(from))
			continue
		}
		switch fv.Verdict {
		case verdictReject:
			vr.rejected = append(vr.rejected, verifyChange{F: it.F, Reason: fv.Reason, Evidence: fv.Evidence})
			drop[finding.ID(it.F.Path, it.F.Title)] = true
			vf.observeVerdict(verdictReject, string(from))
			log.Info("review: verification rejected a finding", "path", it.F.Path, "line", it.F.Line, "severity", from, "reason", mdtext.Shorten(fv.Reason, 200))
			continue
		case verdictDowngrade:
			to := core.Severity(fv.Severity)
			if slices.Contains(finding.AllSeverities, to) && finding.Rank(to) > finding.Rank(from) {
				f := &out.Findings[it.Index]
				f.Severity = to
				f.Body = strings.TrimSpace(f.Body + "\n\n" + downgradeNote(from, fv.Reason))
				vr.downgraded = append(vr.downgraded, verifyChange{F: it.F, To: to, Reason: fv.Reason, Evidence: fv.Evidence})
				vf.observeVerdict(verdictDowngrade, string(from))
				log.Info("review: verification downgraded a finding", "path", it.F.Path, "line", it.F.Line, "from", from, "to", to)
				continue
			}
			log.Info("review: verification: downgrade to the same or a higher severity ignored", "path", it.F.Path, "from", from, "to", mdtext.Shorten(fv.Severity, 20))
		case VerdictConfirm:
		default:
			log.Info("review: verification: unknown verdict ignored", "path", it.F.Path, "verdict", mdtext.Shorten(fv.Verdict, 20))
		}
		vr.confirmed++
		vf.observeVerdict(VerdictConfirm, string(from))
	}
	if len(drop) == 0 {
		return
	}
	kept := out.Findings[:0]
	for _, f := range out.Findings {
		if !drop[finding.ID(f.Path, f.Title)] {
			kept = append(kept, f)
		}
	}
	out.Findings = kept
}

// downgradeNote is appended to a downgraded finding's thread.
func downgradeNote(from core.Severity, reason string) string {
	s := "_🧪 Перепроверка понизила важность (было " + finding.Badge(from) + ")"
	if r := mdtext.OneLine(reason); r != "" {
		s += ": " + mdtext.Shorten(r, 300)
	}
	return s + "_"
}

// RecordRejected stores the rejected findings as outcomes
// (rejected_by_verifier) for the acceptance stats.
func (vf *Feature) RecordRejected(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, vr *Run) {
	if vr == nil || len(vr.rejected) == 0 || !vf.learning.OutcomesOn(eff) {
		return
	}
	ost := vf.learning.OutcomeStore()
	for _, c := range vr.rejected {
		f := c.F
		if _, err := ost.RecordOutcome(ctx, learn.Outcome{
			ProjectPath: job.ProjectPath, MRIID: job.MRIID, FindingID: finding.ID(f.Path, f.Title),
			Title: mdtext.OneLine(f.Title), Severity: string(f.Severity), Path: f.Path, Line: f.Line,
			BodyExcerpt: mdtext.Shorten(f.Body, learning.MaxOutcomeBody), HeadSHAAtPost: mr.DiffRefs.HeadSHA,
			Status: learn.OutcomeRejectedByVerifier, CreatedAt: vf.env.Now(),
		}); err != nil {
			vf.env.Log.Warn("outcomes: record rejected", "mr", job.MRKey(), "err", err)
		}
	}
}

// SummaryBlock is the "🧪 Перепроверка" block ("" when nothing was sent to
// the verification).
func (vr *Run) SummaryBlock() string {
	if vr == nil || len(vr.items)+len(vr.proven) == 0 {
		return ""
	}
	// Paths, titles and the verifier's reasons: defused by summary.Block.
	return summary.Block(marker.VerifyStart+"\n", vr.summaryContent(), marker.VerifyEnd+"\n\n")
}

func (vr *Run) summaryContent() string {
	if len(vr.items) == 0 {
		return vr.provenLine()
	}
	var b strings.Builder
	if vr.failed != "" {
		n := len(vr.items) + vr.overflow
		fmt.Fprintf(&b, "🧪 Перепроверка не выполнена: %s — %d %s без перепроверки.\n", vr.failed, n, publishedWord(n))
		return b.String()
	}
	fmt.Fprintf(&b, "🧪 Перепроверка: подтверждено %d, понижено %d, отклонено %d\n", vr.confirmed, len(vr.downgraded), len(vr.rejected))
	if n := len(vr.downgraded) + len(vr.rejected); n > 0 {
		fmt.Fprintf(&b, "<details>\n<summary>Отклонено и понижено перепроверкой (%d)</summary>\n\n", n)
		for _, c := range vr.rejected {
			b.WriteString("- ❌ отклонено · " + finding.Badge(c.F.Severity) + " · " + changeLine(c))
		}
		for _, c := range vr.downgraded {
			b.WriteString("- ⬇️ понижено · " + finding.Badge(c.F.Severity) + " → " + finding.Badge(c.To) + " · " + changeLine(c))
		}
		b.WriteString("\n</details>\n")
	}
	b.WriteString(vr.provenLine())
	if vr.overflow > 0 {
		fmt.Fprintf(&b, "_Ещё %d %s без перепроверки (лимит `review.verify.max_findings`)._\n", vr.overflow, publishedWord(vr.overflow))
	}
	return b.String()
}

func publishedWord(n int) string {
	return mdtext.Plural(n, "замечание опубликовано", "замечания опубликованы", "замечаний опубликовано")
}

func changeLine(c verifyChange) string {
	s := mdtext.CodeLoc(c.F.Path, c.F.Line) + " — " + mdtext.Shorten(mdtext.OneLine(c.F.Title), 200)
	if r := mdtext.OneLine(c.Reason); r != "" {
		s += " — " + mdtext.Shorten(r, 300)
	}
	return s + "\n"
}
