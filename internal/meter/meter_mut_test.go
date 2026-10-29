package meter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// ctxStore fails like a real database when the context is already done.
type ctxStore struct {
	mu   sync.Mutex
	runs int
}

func (s *ctxStore) RecordRun(ctx context.Context, _ ClaudeRun) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.runs++
	s.mu.Unlock()
	return nil
}
func (s *ctxStore) PruneRuns(ctx context.Context, _ time.Time) (int64, error) { return 0, ctx.Err() }

// A run is recorded even when the job's context was cancelled while Claude
// ran (the tokens are spent): the write gets a live context of its own.
func TestMutRecordsWithLiveContextAfterCancel(t *testing.T) {
	st := &ctxStore{}
	m := Wrap(&fakeRunner{err: context.Canceled}, st, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Run(ctx, core.RunRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if st.runs != 1 {
		t.Fatalf("recorded %d runs, want 1", st.runs)
	}
}

// Pruning runs after the first recorded run, then at most once per 24h,
// deleting runs older than the retention.
func TestMutPruneCadenceAndCutoff(t *testing.T) {
	st := &fakeStore{}
	now := t0
	m := Wrap(&fakeRunner{res: &core.RunResult{}}, st, func() time.Time { return now }, nil, Options{Retention: 48 * time.Hour})
	run := func() {
		if _, err := m.Run(context.Background(), core.RunRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	now = now.Add(24*time.Hour - time.Second)
	run()
	if len(st.prunes) != 1 || !st.prunes[0].Equal(t0.Add(-48*time.Hour)) {
		t.Fatalf("prunes %v", st.prunes)
	}
	now = t0.Add(24 * time.Hour)
	run()
	if len(st.prunes) != 2 || !st.prunes[1].Equal(now.Add(-48*time.Hour)) {
		t.Fatalf("prunes after 24h %v", st.prunes)
	}

	// Negative retention disables pruning.
	st2 := &fakeStore{}
	m2 := Wrap(&fakeRunner{res: &core.RunResult{}}, st2, nil, nil, Options{Retention: -1})
	if _, err := m2.Run(context.Background(), core.RunRequest{}); err != nil || len(st2.prunes) != 0 {
		t.Fatalf("pruned with negative retention: %v %v", st2.prunes, err)
	}
}
