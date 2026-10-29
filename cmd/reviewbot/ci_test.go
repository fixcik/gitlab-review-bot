package main

import (
	"context"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
)

func TestPipelinePollInterval(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitLab.PollInterval = config.Duration(10 * time.Second)
	if d := pipelinePollInterval(&cfg); d != time.Minute {
		t.Errorf("min 1m: %v", d)
	}
	cfg.GitLab.PollInterval = config.Duration(45 * time.Second)
	if d := pipelinePollInterval(&cfg); d != 90*time.Second {
		t.Errorf("2x: %v", d)
	}
}

// Without polling (or with review.ci off) no pipeline poller runs.
func TestStartPipelinePollerOff(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitLab.Events = config.EventsWebhook
	done := startPipelinePoller(context.Background(), &cfg, nil, nil, events.NewRouter(events.Options{}), func(core.Job) {}, nil, nil)
	select {
	case <-done:
	default:
		t.Fatal("webhook mode: the pipeline poller must not run")
	}
	cfg.GitLab.Events = config.EventsPoll
	cfg.Review.CI.Enabled = false
	done = startPipelinePoller(context.Background(), &cfg, nil, nil, events.NewRouter(events.Options{}), func(core.Job) {}, nil, nil)
	select {
	case <-done:
	default:
		t.Fatal("review.ci off: the pipeline poller must not run")
	}
}
