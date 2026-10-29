package main

import (
	"context"
	"log/slog"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/reactions"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
)

// The bot-token client awards emoji (review.reactions).
var _ reactions.API = (*gitlab.Client)(nil)

// newReactions builds the award emoji worker (review.reactions; nil when
// off). api is the bot-token client: reactions are the bot's own, and a
// Reporter may award emoji. Every change is counted
// (reviewbot_reactions_total), recorded as a step of the job's /runs
// timeline when it lands while the job runs, and the first 403 per project
// becomes a reactions_failed problem.
func newReactions(cfg *config.Service, api reactions.API, botID int64, botName string, tm *telemetry.Metrics,
	probs *problems.Service, rec *diag.Recorder, logger *slog.Logger) *reactions.Reactor {
	rc := cfg.Review.Reactions
	if !rc.Enabled {
		logger.Info("reactions off (review.reactions.enabled: false)")
		return nil
	}
	o := reactions.Options{
		API: api, BotID: botID, BotUsername: botName, Logger: logger,
		Observe: func(e reactions.Event, result string) { tm.ObserveReaction(string(e), result) },
		OnForbidden: func(ctx context.Context, project string, err error) {
			detail := ""
			if err != nil {
				detail = err.Error()
			}
			probs.Record(ctx, problems.Problem{Kind: problems.ReactionsFailed, Source: problems.SourceBot, ProjectPath: project,
				Subject: "реакции: GitLab ответил 403 на award emoji", Detail: detail})
		},
	}
	if rec != nil {
		o.Step = func(ctx context.Context, name string) func(string, map[string]any) {
			end := diag.Step(ctx, name)
			return func(status string, f map[string]any) { end(status, diag.Fields(f)) }
		}
	}
	logger.Info("reactions enabled", "picked_up", rc.PickedUp, "done", rc.Done, "thanks", rc.Thanks, "ack", rc.Ack,
		"clean_review", rc.CleanReview, "ignore_users", len(rc.IgnoreUsers))
	return reactions.New(o)
}
