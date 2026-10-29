package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/poller"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// pipelinePollInterval: how often the pipelines of every project are
// listed when polling (twice the MR poll interval, at least a minute: one
// API call per project per tick).
func pipelinePollInterval(cfg *config.Service) time.Duration {
	return max(2*cfg.GitLab.PollInterval.D(), time.Minute)
}

// startPipelinePoller polls the pipelines of the group projects for the CI
// failure digest (review.ci with gitlab.events poll/both). The returned
// channel is closed when it has stopped (at once when it does not run).
func startPipelinePoller(ctx context.Context, cfg *config.Service, gl *gitlab.Client, st *store.SQLite, router *events.Router,
	enqueue func(core.Job), tr *tracing.Tracer, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	if !cfg.Review.CI.Enabled || !cfg.GitLab.UsesPolling() {
		close(done)
		return done
	}
	p := poller.NewPipelines(poller.PipelineOptions{
		Source: gl, State: st, Router: router, Enqueue: enqueue, Groups: cfg.GitLab.Groups,
		Interval: pipelinePollInterval(cfg), Logger: logger, Tracer: tr,
	})
	go func() { defer close(done); p.Run(ctx) }()
	return done
}
