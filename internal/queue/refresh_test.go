package queue

import (
	"context"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// JobRefreshTitle is not an auto job: it must run without the debounce delay.
func TestRefreshTitleRunsImmediately(t *testing.T) {
	q := New(Options{Concurrency: 1, Debounce: time.Hour})
	var rec recorder
	start(t, q, func(ctx context.Context, j core.Job) error { rec.add(j); return nil })
	q.Enqueue(core.Job{Kind: core.JobRefreshTitle, ProjectID: 1, MRIID: 3})
	waitFor(t, time.Second, func() bool { return len(rec.get()) == 1 })
	if rec.get()[0].Kind != core.JobRefreshTitle {
		t.Fatalf("jobs %+v", rec.get())
	}
}
