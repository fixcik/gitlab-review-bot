package diag

import (
	"context"
	"fmt"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// pending is a started step; done/end finish it. A nil *pending is a no-op.
// run is nil when only the span is recorded (tracing without diagnostics).
type pending struct {
	run   *Run
	idx   int
	start time.Time
	span  *tracing.Span
}

func (run *Run) begin(kind, name string, f Fields) *pending {
	if run == nil {
		return nil
	}
	now := run.rec.o.Now()
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.finished {
		return nil
	}
	n := len(run.r.Steps) + 1
	run.r.Steps = append(run.r.Steps, TimelineStep{N: n, Kind: kind, Name: run.rec.scrub.str(name, run.safe), Start: now,
		Status: StepRunning, Fields: run.cleanFields(f)})
	return &pending{run: run, idx: n - 1, start: now}
}

// event adds a finished zero-length step.
func (run *Run) event(kind, name, status, detail string, f Fields) {
	p := run.begin(kind, name, f)
	p.end(status, detail, nil)
}

// end finishes the step.
func (p *pending) end(status, detail string, f Fields) {
	if p == nil {
		return
	}
	p.endSpan(status, nil, f)
	if p.run == nil {
		return
	}
	p.finish(func(s *TimelineStep) {
		s.Status = status
		if detail != "" {
			s.Detail = p.run.rec.scrub.str(detail, p.run.safe)
		}
		p.run.mergeFields(s, f)
	})
}

// done finishes the step as ok or error.
func (p *pending) done(err error, f Fields) {
	if p == nil {
		return
	}
	if err != nil {
		p.endSpan(StepError, err, f)
		if p.run == nil {
			return
		}
		p.finish(func(s *TimelineStep) {
			s.Status = StepError
			s.Error = p.run.rec.scrub.str(err.Error(), p.run.safe)
			p.run.mergeFields(s, f)
		})
		return
	}
	p.end(StepOK, "", f)
}

func (p *pending) finish(fn func(*TimelineStep)) {
	if p == nil || p.run == nil {
		return
	}
	run := p.run
	now := run.rec.o.Now()
	run.mu.Lock()
	defer run.mu.Unlock()
	if p.idx >= len(run.r.Steps) {
		return
	}
	s := &run.r.Steps[p.idx]
	s.DurationMS = now.Sub(p.start).Milliseconds()
	fn(s)
	run.flushLocked(false)
}

// with edits the step under the lock (outputs).
func (p *pending) with(fn func(run *Run, s *TimelineStep)) {
	if p == nil || p.run == nil {
		return
	}
	p.run.mu.Lock()
	defer p.run.mu.Unlock()
	if p.idx < len(p.run.r.Steps) {
		fn(p.run, &p.run.r.Steps[p.idx])
	}
}

func (run *Run) mergeFields(s *TimelineStep, f Fields) {
	if len(f) == 0 {
		return
	}
	if s.Fields == nil {
		s.Fields = Fields{}
	}
	for k, v := range run.cleanFields(f) {
		s.Fields[k] = v
	}
}

// cleanFields scrubs string values (and string slices).
func (run *Run) cleanFields(f Fields) Fields {
	if len(f) == 0 {
		return nil
	}
	out := make(Fields, len(f))
	for k, v := range f {
		switch x := v.(type) {
		case string:
			out[k] = run.rec.scrub.str(x, run.safe)
		case []string:
			out[k] = run.scrubAll(x)
		case error:
			out[k] = run.rec.scrub.str(x.Error(), run.safe)
		case time.Duration:
			out[k] = x.String()
		case fmt.Stringer:
			out[k] = run.rec.scrub.str(x.String(), run.safe)
		default:
			out[k] = v
		}
	}
	return out
}

// scrubAll scrubs every string of ss into a new slice.
func (run *Run) scrubAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = run.rec.scrub.str(s, run.safe)
	}
	return out
}

// Step starts a named step of the job in ctx and returns the function that
// ends it with a status (StepOK, StepError, ...) and optional fields. A
// traced job also gets a span named name. Without a Run or a span in ctx
// it is a no-op.
func Step(ctx context.Context, name string) func(status string, f Fields) {
	_, p := startStep(ctx, name, KindStep, name, nil)
	return func(status string, f Fields) { p.end(status, "", f) }
}

// Note adds an instant step (a fact worth seeing in the timeline).
func Note(ctx context.Context, name string, f Fields) {
	FromContext(ctx).event(KindStep, name, StepInfo, "", f)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
