package diag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestOutcomeMatchesFinish(t *testing.T) {
	e := newEnv(t)
	logger := e.rec.Logger(slog.New(slog.DiscardHandler))
	cases := []struct {
		h    core.Handler
		want string
	}{
		{func(ctx context.Context, j core.Job) error { return nil }, StatusOK},
		{func(ctx context.Context, j core.Job) error {
			logger.With("mr", j.MRKey()).Info("review: draft mr, skipping")
			return nil
		}, StatusSkipped},
		{func(ctx context.Context, j core.Job) error {
			return fmt.Errorf("wrapped: %w", &core.RetryLater{At: time.Now(), Reason: "x"})
		}, StatusPostponed},
		{func(ctx context.Context, j core.Job) error { return context.Canceled }, StatusInterrupted},
		{func(ctx context.Context, j core.Job) error { return errors.New("boom") }, StatusFailed},
	}
	for _, tc := range cases {
		var got, id string
		h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
			err := tc.h(ctx, j)
			got, id = FromContext(ctx).Outcome(err), FromContext(ctx).ID()
			return err
		})
		_ = h(context.Background(), job)
		row, _ := e.record(t, id)
		if got != tc.want || row.Status != tc.want {
			t.Errorf("outcome %q, recorded %q, want %q", got, row.Status, tc.want)
		}
	}
	var nilRun *Run
	if nilRun.Outcome(nil) != StatusOK || Outcome(errors.New("x")) != StatusFailed {
		t.Fatal("nil run outcome")
	}
}
