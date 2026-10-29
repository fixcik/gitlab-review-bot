package reviewtest

import (
	"context"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/meter"
)

type RunLog struct {
	mu   sync.Mutex
	Runs []meter.ClaudeRun
}

func (l *RunLog) RecordRun(_ context.Context, r meter.ClaudeRun) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Runs = append(l.Runs, r)
	return nil
}

func (l *RunLog) PruneRuns(context.Context, time.Time) (int64, error) { return 0, nil }

// metered wraps the harness runner with the usage meter.
func Metered(h *Harness) *RunLog {
	l := &RunLog{}
	h.Deps.Runner = meter.Wrap(h.Runner, l, func() time.Time { return TestNow }, nil)
	return l
}
