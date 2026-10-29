package review

// A small dependency-aware step runner for the review's analyzers
// (analyzers.go): stdlib only (sync.WaitGroup, channels).
//
// Steps are added in the order of the sequential flow, each with the steps
// it depends on (added before it). Parallel: every step runs in its own
// goroutine as soon as its dependencies are done, at most maxParallel at a
// time. Sequential (review.parallel_analyzers: false): add runs the step
// right away, exactly like the code before.
//
// Each step gets the group's context (its spans and diag steps nest under
// the review span as before) with a problems buffer: what the step records
// with jobctx.BotProblem is added to the job's problems when wait returns, in the
// order the steps were added, so the summary lists problems in the same order
// whatever ran first. A panic in a step is recovered in its goroutine and
// raised again by wait on the job's goroutine (as the sequential flow
// would), after every step has finished.

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
)

// maxParallel bounds how many analyzer steps of one review run at the same
// time (they are mostly I/O: git, docker exec of a checker, HTTP).
const maxParallel = 4

type stepGroup struct {
	ctx      context.Context
	parallel bool
	sem      chan struct{}
	wg       sync.WaitGroup
	steps    []*step
}

type step struct {
	name  string
	done  chan struct{}
	buf   *jobctx.ProblemBuf
	panic any
	stack []byte
}

func newStepGroup(ctx context.Context, parallel bool) *stepGroup {
	return &stepGroup{ctx: ctx, parallel: parallel, sem: make(chan struct{}, maxParallel)}
}

// add schedules fn after deps. fn must not fail the review: it records its
// own problems and returns its result through the closure.
func (g *stepGroup) add(name string, fn func(ctx context.Context), deps ...*step) *step {
	s := &step{name: name, done: make(chan struct{}), buf: &jobctx.ProblemBuf{}}
	g.steps = append(g.steps, s)
	if !g.parallel {
		defer close(s.done)
		fn(g.ctx) // problems go straight to the job; a panic propagates as before
		return s
	}
	ctx := jobctx.WithProblemBuf(g.ctx, s.buf)
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer close(s.done)
		defer func() {
			if p := recover(); p != nil {
				s.panic, s.stack = p, debug.Stack()
			}
		}()
		for _, d := range deps {
			<-d.done // every step closes done, cancelled or not
		}
		select {
		case g.sem <- struct{}{}:
		case <-ctx.Done():
			return // the job is cancelled: the caller checks ctx after wait
		}
		defer func() { <-g.sem }()
		fn(ctx)
	}()
	return s
}

// wait waits for every step, adds their problems to the job in the order
// the steps were added and re-raises the first panic.
func (g *stepGroup) wait() {
	g.wg.Wait()
	for _, s := range g.steps {
		s.buf.Flush(g.ctx)
	}
	for _, s := range g.steps {
		if s.panic != nil {
			panic(fmt.Sprintf("review step %s: %v\n%s", s.name, s.panic, s.stack))
		}
	}
}
