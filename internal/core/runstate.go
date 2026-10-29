package core

import "context"

// RunState is what the queue knows about the attempt of a job that is
// running now, reachable from the handler's context (RunStateFrom). The
// queue implements it (internal/queue); it is the seam another job runner
// implements to keep the same accounting and replay rules.
type RunState interface {
	// Job is the running job as the queue dispatched it.
	Job() Job
	// Admitted reports whether an earlier attempt of this job was admitted
	// and charged against its author's limits: the job resumes after a
	// *RetryLater (in process or after a restart) or is replayed after a
	// restart interrupted it. Such an attempt skips the access check and the
	// per-user limits; its runs still count globally. Once true it stays true
	// for every later attempt.
	Admitted() bool
	// MarkCharged records that this attempt committed a Claude run against
	// the job author's limits.
	MarkCharged()
	// MarkAnswered records that this attempt posted the job's final reply
	// (review/env Reply; a replay after a restart would answer twice).
	MarkAnswered()
}

type runStateKey struct{}

// WithRunState returns ctx carrying the running job's state.
func WithRunState(ctx context.Context, s RunState) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, runStateKey{}, s)
}

// RunStateFrom returns the running job's state (nil outside a queue job).
func RunStateFrom(ctx context.Context) RunState {
	s, _ := ctx.Value(runStateKey{}).(RunState)
	return s
}

// RunAdmitted reports whether ctx runs an attempt of a job whose earlier
// attempt was already admitted and charged (RunState.Admitted; false
// outside a queue job).
func RunAdmitted(ctx context.Context) bool {
	s := RunStateFrom(ctx)
	return s != nil && s.Admitted()
}
