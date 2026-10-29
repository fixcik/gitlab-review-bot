package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/fixcik/gitlab-review-bot/internal/claude"
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/jira"
	"github.com/fixcik/gitlab-review-bot/internal/repo"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
)

// The GitLab client provides CI jobs and artifacts for diff coverage.
var _ cijob.Artifacts = (*gitlab.Client)(nil)

// The GitLab client lists open MRs of other projects (system map phase B).
var _ crossrepo.OpenMRs = (*gitlab.Client)(nil)

// The GitLab client reads failed pipelines, main branch pipelines and
// comments on commits (review.ci).
var (
	_ pipeline.CIFailures     = (*gitlab.Client)(nil)
	_ pipeline.CIMain         = (*gitlab.Client)(nil)
	_ pipeline.CommitComments = (*gitlab.Client)(nil)
)

// newGitLabClient builds a client for token with the shared options (logger,
// CA file) and the given write policy.
func newGitLabClient(cfg *config.Service, token string, writes gitlab.WritePolicy, logger *slog.Logger) (*gitlab.Client, error) {
	opts := []gitlab.Option{gitlab.WithLogger(logger), gitlab.WithWrites(writes)}
	if cfg.GitLab.CAFile != "" {
		opts = append(opts, gitlab.WithCAFile(cfg.GitLab.CAFile))
	}
	return gitlab.New(cfg.GitLab.URL, token, opts...)
}

// botUser is the GITLAB_TOKEN account; the bot answers to its username.
type botUser struct {
	id      int64
	name    string
	mention string
}

// connectBot builds the bot's own client and reads its account. The client
// writes MR comments and MR labels (labels: 403 for a Reporter; the service
// client is used then, see review/labels/gitlab.go).
func connectBot(ctx context.Context, cfg *config.Service, tm *telemetry.Metrics, logger *slog.Logger) (*gitlab.Client, botUser, error) {
	gl, err := newGitLabClient(cfg, cfg.GitLab.Token, gitlab.BotWrites, logger)
	if err != nil {
		return nil, botUser{}, err
	}
	observeGitLab(tm, gl)
	id, name, err := gl.CurrentUser(ctx)
	if err != nil {
		return nil, botUser{}, fmt.Errorf("gitlab: cannot read bot user (check GITLAB_TOKEN): %w", err)
	}
	// The mention always follows the GITLAB_TOKEN account's username.
	bot := botUser{id: id, name: name, mention: "@" + name}
	if cfg.Review.BotMention != "" && cfg.Review.BotMention != bot.mention {
		logger.Warn("review.bot_mention is no longer supported and is ignored: the bot answers to its GitLab username",
			"configured", cfg.Review.BotMention, "mention", bot.mention)
	}
	logger.Info("gitlab connected", "bot", name, "mention", bot.mention)
	return gl, bot, nil
}

// newOptionalServiceClient builds the service-token client, or returns nil
// when GITLAB_SERVICE_TOKEN is not set.
func newOptionalServiceClient(cfg *config.Service, tm *telemetry.Metrics, logger *slog.Logger) (*gitlab.Client, error) {
	if cfg.GitLab.ServiceToken == "" {
		logger.Info("no GITLAB_SERVICE_TOKEN: the bot will not assign itself as reviewer, set MR labels or close the threads of people's commands (it resolves its own threads with its own token)")
		return nil, nil
	}
	svc, err := newServiceClient(cfg, logger)
	if err != nil {
		return nil, err
	}
	observeGitLab(tm, svc)
	return svc, nil
}

// newRepos builds the checkout manager.
func newRepos(cfg *config.Service, dataDir string, logger *slog.Logger) (*repo.Manager, error) {
	return repo.New(repo.Options{
		BaseDir:   filepath.Join(dataDir, "repos"),
		GitLabURL: cfg.GitLab.URL,
		Token:     cfg.GitLab.Token,
		CAFile:    cfg.GitLab.CAFile,
		Logger:    logger,
		// Secrets referenced by tracing.headers reach no host git either.
		HideEnv: cfg.Tracing.HeaderEnv(),
		// Shared read-only checkouts kept after use (review.checkout_cache).
		CacheTTL:      cfg.Review.CheckoutCache.CacheTTL(),
		CacheMaxBytes: cfg.Review.CheckoutCache.MaxBytes(),
	})
}

// newRunner builds the Claude runner (sandbox.*) with its decorators
// (instrumentRunner: token accounting for /usage, diagnostics steps and
// spans, and metrics).
func newRunner(cfg *config.Service, st *store.SQLite, in instruments, logger *slog.Logger) (core.Runner, error) {
	runner, err := claude.NewRunner(claude.Options{
		Mode:        cfg.Sandbox.Mode,
		Image:       cfg.Sandbox.Image,
		Network:     cfg.Sandbox.Network,
		EgressProxy: cfg.Sandbox.EgressProxy,
		CPUs:        cfg.Sandbox.CPUs,
		Memory:      cfg.Sandbox.Memory,
		Logger:      logger,
		// Experimental Claude Code trace export (tracing.claude_cli).
		Telemetry: cliTelemetry(cfg, logger),
		// Secrets referenced by tracing.headers reach no child process.
		HideEnv: cfg.Tracing.HeaderEnv(),
	})
	if err != nil {
		return nil, err
	}
	return instrumentRunner(in, cfg, runner, st, logger), nil
}

// newTaskSource builds the Jira client (a nil interface when Jira is not
// configured).
func newTaskSource(cfg *config.Service, logger *slog.Logger) (task.Source, error) {
	if !cfg.Jira.Enabled() {
		return nil, nil
	}
	jc, err := jira.New(cfg.Jira.URL, cfg.Jira.Token,
		jira.WithCAFile(cfg.Jira.CAFile), jira.WithLogger(logger),
		jira.WithCommentsLimit(cfg.Jira.CommentsLimit), jira.WithEpicLinkField(cfg.Jira.EpicLinkField))
	if err != nil {
		return nil, err
	}
	logger.Info("jira enabled", "url", cfg.Jira.URL, "project_keys", cfg.Jira.ProjectKeys)
	return jc, nil
}
