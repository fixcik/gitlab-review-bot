package queue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestStats(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	q := New(Options{Concurrency: 1, Debounce: time.Hour, Now: clock})

	q.Enqueue(core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 1}) // debounced: pending
	if s := q.Stats(); s != (Stats{Pending: 1}) {
		t.Fatalf("after enqueue: %+v", s)
	}

	release := make(chan struct{})
	started := make(chan struct{}, 4)
	calls := 0
	h := func(ctx context.Context, j core.Job) error {
		started <- struct{}{}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			<-release
			return nil
		}
		return &core.RetryLater{At: clock().Add(time.Hour), Reason: "limit"}
	}
	start(t, q, h)
	q.Enqueue(core.Job{Kind: core.JobReview, ProjectID: 2, MRIID: 1}) // manual: runs now
	<-started
	if s := q.Stats(); s != (Stats{Pending: 1, Running: 1}) {
		t.Fatalf("running: %+v", s)
	}
	q.Enqueue(core.Job{Kind: core.JobChat, ProjectID: 3, MRIID: 1}) // waits for the worker
	close(release)
	<-started // chat job runs and asks for a retry
	waitFor(t, time.Second, func() bool { return q.Stats() == (Stats{Pending: 1, RetryWait: 1}) })

	mu.Lock()
	now = now.Add(2 * time.Hour) // retry time passed: counts as pending again
	mu.Unlock()
	if s := q.Stats(); s.RetryWait != 0 {
		t.Fatalf("after retry time: %+v", s)
	}
}
