package queue

// Stats is a point-in-time view of the queue, for metrics.
type Stats struct {
	// Pending jobs waiting to run (debounce, busy workers, another job of
	// the same MR running, or a retry whose time has come). A job waiting
	// for its retry time never holds others back (pickLocked), so a job
	// counted here runs as soon as a worker and its MR are free.
	Pending int
	// Running handlers.
	Running int
	// RetryWait: jobs re-scheduled by *core.RetryLater whose retry time is
	// still in the future (not counted in Pending).
	RetryWait int
	// Draining: the queue was told to stop (SIGTERM/SIGINT) and waits for
	// its running jobs (Options.ShutdownGrace); nothing new starts.
	Draining bool
}

// Stats returns the current queue counters. Read-only, safe for concurrent
// use.
func (q *Queue) Stats() Stats {
	now := q.now()
	q.mu.Lock()
	defer q.mu.Unlock()
	s := Stats{Running: q.nRunning, Draining: q.draining}
	for _, e := range q.pending {
		if e.retry && e.readyAt.After(now) {
			s.RetryWait++
		} else {
			s.Pending++
		}
	}
	return s
}
