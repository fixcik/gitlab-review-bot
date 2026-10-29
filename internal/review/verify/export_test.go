package verify

import (
	"context"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// Unexported names the external tests (package verify_test) use.

// RenderVerification is renderVerification.
var RenderVerification = renderVerification

// DiffExcerpt is diffExcerpt.
var DiffExcerpt = diffExcerpt

// Run results and why the findings were not verified.
const (
	ResultLimit   = resultLimit
	ResultClaude  = resultClaude
	ResultInvalid = resultInvalid
	ResultError   = resultError

	WhyLimit   = whyLimit
	WhyClaude  = whyClaude
	WhyInvalid = whyInvalid
	WhyError   = whyError
)

// ClaimsContext and ClaimsRules are the claim rules.
const (
	ClaimsContext = claimsContext
	ClaimsRules   = claimsRules
)

// SelectFindings is selectFindings.
func (vf *Feature) SelectFindings(fs []output.Finding, threads *mrthread.Threads, diff string, proven func(output.Finding) string) ([]Item, int, []provenFinding) {
	return vf.selectFindings(fs, threads, diff, proven)
}

// PlanCompat is the compat-only verification of a review's breaking-change
// claims (what BeforePublish does when the verification of findings is off):
// every deterministic change and every Claude item already discussed go to
// cr.Items, Claude's new items through ONE verification run.
func (vf *Feature) PlanCompat(ctx context.Context, job core.Job, cr *compatcheck.Run, out *output.ReviewOutput, threads *mrthread.Threads, dir, from, head string, lang string) {
	if cr == nil || !cr.Enabled {
		return
	}
	toVerify, apiCtx := compatcheck.Candidates(cr, out, threads)
	res := vf.runVerification(ctx, job, Input{Dir: dir, From: from, Head: head, Lang: lang, CtxDirs: cr.CtxDirs, apiCtx: apiCtx, Compat: toVerify})
	cr.AddVerified(res.compatPublish, res.compatRejected)
}
