package queue

import (
	"context"
	"sync"
	"time"
)

// Drain outcomes (the "drain" field of the "queue: stopped" log line).
const (
	DrainIdle      = "idle"      // nothing was running
	DrainFinished  = "finished"  // every running job finished within the grace
	DrainCancelled = "cancelled" // grace 0, over or aborted: running jobs were cancelled
)

// drain waits for the running handlers: up to the grace period, then (or
// on Abort, or at once with grace 0) it cancels them and waits for them to
// return.
func (q *Queue) drain(wg *sync.WaitGroup, cancel context.CancelFunc) string {
	q.mu.Lock()
	running := q.nRunning
	q.mu.Unlock()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	if running == 0 {
		<-done
		return DrainIdle
	}
	if q.grace > 0 {
		q.log.Info("queue: draining: waiting for running jobs", "running", running, "grace", q.grace)
		t := time.NewTimer(q.grace)
		defer t.Stop()
		select {
		case <-done:
			q.log.Info("queue: drain finished: running jobs completed")
			return DrainFinished
		case <-t.C:
			q.log.Warn("queue: drain grace over: cancelling running jobs (they run again after the restart)", "grace", q.grace)
		case <-q.abort:
			q.log.Warn("queue: drain aborted: cancelling running jobs (they run again after the restart)")
		}
	}
	cancel()
	<-done
	return DrainCancelled
}
