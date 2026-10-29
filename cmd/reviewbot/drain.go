package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
)

// runQueue runs q with h until ctx is done (the signal) or the HTTP
// listener fails (listen delivers its error), then drains the queue the
// same way in both cases — nothing new starts, running jobs (restored ones
// included) get grace to finish, a second signal cancels them (they keep
// their rows) — and returns once Run has returned: the listener's error,
// else Run's. The caller stops the event sources and closes the store only
// after it (a listener failure must not close the store under running
// jobs).
func runQueue(ctx context.Context, q *queue.Queue, h core.Handler, listen <-chan error, grace time.Duration, logger *slog.Logger) error {
	qCtx, stopQueue := context.WithCancel(ctx)
	defer stopQueue()
	qDone := make(chan error, 1)
	go func() { qDone <- q.Run(qCtx, h) }()

	var listenErr error
	select {
	case <-ctx.Done():
	case listenErr = <-listen:
	}
	if listenErr != nil {
		logger.Error("listener failed: stopping", "err", listenErr)
	}
	logger.Info("shutting down: draining the queue", "grace", grace)
	stopQueue()
	stopAbort := abortOnSignal(q, logger)
	err := <-qDone
	stopAbort()
	if listenErr != nil {
		return listenErr
	}
	return err
}

// warnEventsWebhook: with gitlab.events "webhook" alone, events that arrive
// while the bot is down or restarting are lost (GitLab does not redeliver a
// failed webhook); "both" catches them up by polling after the start.
func warnEventsWebhook(cfg *config.Service, logger *slog.Logger) {
	if cfg.GitLab.Events != config.EventsWebhook {
		return
	}
	logger.Warn(`gitlab.events is "webhook": events delivered while the bot is down or restarting are lost (GitLab does not retry failed webhooks); use "both" in production so polling catches them up after a restart (needs gitlab.groups)`)
}

// abortOnSignal: a second SIGINT/SIGTERM during the drain cancels the
// running jobs at once (q.Abort) instead of waiting for the rest of
// queue.shutdown_grace; they keep their rows and run again after the start.
// The returned func stops listening.
func abortOnSignal(q *queue.Queue, logger *slog.Logger) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case s := <-ch:
			logger.Warn("second signal: cancelling running jobs now", "signal", s.String())
			q.Abort()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
