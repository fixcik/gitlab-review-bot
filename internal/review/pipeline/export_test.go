package pipeline

import (
	"context"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
)

// Aliases for the package's external tests (package pipeline_test).

type (
	CIDigest      = ciDigest
	CIJobDigest   = ciJobDigest
	CITestCase    = ciTestCase
	CIExplanation = ciExplanation
	CIJobCause    = ciJobCause
	CILoc         = ciLoc
	CICandidate   = ciCandidate
	CIPromptData  = ciPromptData
	CIThreadState = ciThreadState
)

const (
	CIKindTests     = ciKindTests
	CIKindLint      = ciKindLint
	CIKindLog       = ciKindLog
	TBDRefreshEvery = tbdRefreshEvery
	TBDRefreshFor   = tbdRefreshFor
)

var (
	CIBlockPipeline     = ciBlockPipeline
	CISchema            = ciSchema
	CISystemTmpl        = ciSystemTmpl
	CIUserTmpl          = ciUserTmpl
	CleanCILog          = cleanCILog
	MatchJobs           = matchJobs
	RenderCIBlock       = renderCIBlock
	RenderCIDigest      = renderCIDigest
	RenderCIExplanation = renderCIExplanation
	RenderCIThread      = renderCIThread
	ReplaceCI           = replaceCI
)

func (pf *Feature) BuildCIDigest(ctx context.Context, projectID int64, p core.Pipeline, eff config.Effective) (*CIDigest, error) {
	return pf.buildCIDigest(ctx, projectID, p, eff)
}

func (pf *Feature) RenderMainNote(job core.Job, d *CIDigest, expl *CIExplanation, culprit *gitlab.CommitMR, cands []gitlab.CommitMR, branch string, eff config.Effective) string {
	return pf.renderMainNote(job, d, expl, culprit, cands, branch, eff)
}

func (pf *Feature) UpsertCIThread(ctx context.Context, job core.Job, th *mrthread.CIThread, body string) {
	pf.upsertCIThread(ctx, job, th, body)
}
