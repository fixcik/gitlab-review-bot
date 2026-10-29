package linters

import (
	"context"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// Gate tells whether a Claude finding of a severity and a lint result
// block the merge (implemented by *Feature).
type Gate interface {
	Blocking(s core.Severity) bool
	ResultBlocking(f lint.Finding) bool
}

// Blocking is env.Blocking (Gate).
func (lf *Feature) Blocking(s core.Severity) bool { return lf.env.Blocking(s) }

// RecountGate rescans the MR threads and rewrites the merge-gate line of a
// summary body (body unchanged on error or without a gate line).
func (lf *Feature) RecountGate(ctx context.Context, job core.Job, summaryID int64, body string) (string, error) {
	t, err := lf.pub.Scan(ctx, job.ProjectID, job.MRIID, summaryID)
	if err != nil {
		return body, err
	}
	blocking, pending := GateCount(t)
	if nb, ok := summary.SetGateLine(body, summary.GateLine(blocking, pending)); ok {
		return nb, nil
	}
	return body, nil
}

// GateCount counts the open merge-blocking bot threads like the review
// does (findings, compat, lint, the size thread and any other thread with
// the blocking marker, fixed but not yet resolved).
func GateCount(t *mrthread.Threads) (blocking, fixedPending int) {
	blocking = len(t.OpenCompat) + t.OpenLintBlocking + t.FixedPendingBlocking + t.OpenSizeBlocking + t.OpenOtherBlocking
	for _, f := range t.Open {
		if f.Blocking {
			blocking++
		}
	}
	return blocking, t.FixedPendingBlocking
}

// ResultBlocking reports whether a lint result blocks the merge (Gate).
func (lf *Feature) ResultBlocking(f lint.Finding) bool {
	switch {
	case f.Secret():
		return true
	case f.MayBlock: // CI linter marked blocking
		return lf.env.Blocking(f.Severity)
	case lint.IsLinter(f.Tool):
		return false
	default:
		return lf.env.Blocking(f.Severity)
	}
}
