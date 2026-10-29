package queue

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Run dispatches jobs to h until ctx is cancelled, then drains: no new job
// starts, running handlers get Options.ShutdownGrace to finish (Abort ends
// the wait early), then their context is cancelled and Run waits for them to
// return. Handlers run with a context that carries ctx's values but is
// cancelled only by the drain. Pending jobs are not run; with
// Options.Persist they are reloaded by the next process.
func (q *Queue) Run(ctx context.Context, h core.Handler) error {
	if h == nil {
		return errors.New("queue: nil handler")
	}
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return errors.New("queue: Run called twice")
	}
	q.started = true
	q.restoreLocked()
	q.mu.Unlock()

	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()
	var wg sync.WaitGroup
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	q.log.Info("queue: started", "concurrency", q.concurrency, "debounce", q.debounce, "shutdown_grace", q.grace)
	for {
		q.mu.Lock()
		if ctx.Err() != nil {
			q.draining = true
			q.mu.Unlock()
			break
		}
		now := q.now()
		for q.nRunning < q.concurrency {
			e := q.pickLocked(now)
			if e == nil {
				break
			}
			q.running[e.key] = true
			q.nRunning++
			q.markStartedLocked(e, now)
			wg.Add(1)
			go func() {
				defer wg.Done()
				q.runJob(jobCtx, h, e)
			}()
		}
		next := q.nextWakeLocked(now)
		q.mu.Unlock()

		if !next.IsZero() {
			timer.Reset(max(next.Sub(now), 0))
		} else {
			timer.Stop()
		}
		select {
		case <-ctx.Done():
		case <-q.wake:
		case <-timer.C:
		}
	}

	outcome := q.drain(&wg, cancelJobs)
	// Pending jobs stay in memory (not dispatched): an Enqueue after Run
	// (webhooks until the HTTP server stops) still debounces over them and
	// merges ForceFull into the persisted row; the persisted rows are what
	// the next process restores.
	q.mu.Lock()
	left := len(q.pending)
	q.started = false
	q.draining = false
	q.mu.Unlock()
	q.log.Info("queue: stopped", "drain", outcome, "pending_left", left, "persisted", q.persist != nil)
	return nil
}

func (q *Queue) runJob(jobCtx context.Context, h core.Handler, e *entry) {
	start := time.Now()
	log := q.log.With("mr", e.key, "kind", e.job.Kind, "project", e.job.ProjectPath)
	log.Debug("queue: job started")

	err := q.call(withRun(jobCtx, q, e), h, e.job)

	q.mu.Lock()
	delete(q.running, e.key)
	q.nRunning--
	retried, interrupted := q.afterRunLocked(jobCtx, e, err, log)
	q.mu.Unlock()
	q.signal()

	switch {
	case retried:
	case interrupted:
		log.Warn("queue: job interrupted by shutdown, kept for the restart", "err", err, "duration", time.Since(start),
			"persisted", q.persist != nil)
	case err != nil:
		log.Error("queue: job failed", "err", err, "duration", time.Since(start))
	default:
		log.Debug("queue: job done", "duration", time.Since(start))
	}
}

func (q *Queue) call(ctx context.Context, h core.Handler, job core.Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			q.log.Error("queue: handler panicked", "mr", job.MRKey(), "kind", job.Kind,
				"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	return h(ctx, job)
}
