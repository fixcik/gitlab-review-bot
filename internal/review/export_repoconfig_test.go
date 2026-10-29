package review

import (
	"context"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// ReviewConfigGate is reviewConfigGate (the repository config step of a
// review): the summary line, the merged language, whether the review goes
// on, and the GitLab error.
func (r *Reviewer) ReviewConfigGate(ctx context.Context, job core.Job, mr *core.MergeRequest) (summaryLine, language string, ok bool, err error) {
	g, ok, err := r.reviewConfigGate(ctx, job, mr)
	return g.summaryLine, g.eff.Language, ok, err
}
