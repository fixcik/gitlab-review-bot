package diag

import (
	"context"
	"errors"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Outcome classifies a job handler's result with the rules of Run.Finish,
// minus the skip detection (which needs the Run): nil -> ok,
// *core.RetryLater -> postponed, context.Canceled -> interrupted, anything
// else -> failed. Used by the metrics, which must agree with /runs.
func Outcome(err error) string {
	var rl *core.RetryLater
	switch {
	case err == nil:
		return StatusOK
	case errors.As(err, &rl):
		return StatusPostponed
	case errors.Is(err, context.Canceled):
		return StatusInterrupted
	default:
		return StatusFailed
	}
}

// Outcome is the status Finish(err) records for this run, including the
// skip detection (ok -> skipped when the job logged a skip reason and ran no
// Claude session). It can be called before Finish. A nil Run (diagnostics
// off) falls back to the package-level Outcome.
func (run *Run) Outcome(err error) string {
	st := Outcome(err)
	if run == nil || st != StatusOK {
		return st
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.skippedLocked() {
		return StatusSkipped
	}
	return st
}

// skippedLocked: the job logged a skip reason and ran no Claude session.
// Called with run.mu held.
func (run *Run) skippedLocked() bool {
	return run.skip != "" && len(run.r.Claude) == 0
}
