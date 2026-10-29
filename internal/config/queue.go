package config

import "time"

// Queue configures the job queue's durability and shutdown (service config
// only; see docs/DESIGN.md "Queue durability").
type Queue struct {
	// ShutdownGrace: on SIGTERM/SIGINT the queue stops starting jobs and
	// gives the running ones this long to finish (a running Claude run is
	// not thrown away); then it cancels them (they keep their rows and run
	// again after the restart). Webhooks and polling keep being accepted
	// and persisted meanwhile. 0 = cancel at once. Default 5m, at most 1h.
	// The container stop timeout (compose stop_grace_period, Kubernetes
	// terminationGracePeriodSeconds) must be longer.
	ShutdownGrace Duration `yaml:"shutdown_grace"`
	// ManualMaxAge: on restart, a persisted job that is not an automatic
	// review (command, question, thread reply, refresh, pipeline) enqueued
	// longer ago than this is dropped instead of run (an answer hours late
	// is noise). Default 6h; must be > 0.
	ManualMaxAge Duration `yaml:"manual_max_age"`
}

// Queue defaults and bounds.
const (
	DefaultShutdownGrace = 5 * time.Minute
	MaxShutdownGrace     = time.Hour
	DefaultManualMaxAge  = 6 * time.Hour
)

func defaultQueue() Queue {
	return Queue{ShutdownGrace: Duration(DefaultShutdownGrace), ManualMaxAge: Duration(DefaultManualMaxAge)}
}

func (q Queue) validate() []string {
	var errs []string
	if q.ShutdownGrace < 0 || q.ShutdownGrace.D() > MaxShutdownGrace {
		errs = append(errs, "queue.shutdown_grace must be between 0 and 1h")
	}
	if q.ManualMaxAge.D() <= 0 {
		errs = append(errs, "queue.manual_max_age must be > 0")
	}
	return errs
}
