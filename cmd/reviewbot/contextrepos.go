package main

import (
	"context"
	"log/slog"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/contextrepos"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/repo"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
)

// startContextRepos sets up read-only context repositories (the other
// projects of gitlab.groups) and keeps their snapshots warm in the
// background. It returns a nil interface when the feature is off.
func startContextRepos(ctx context.Context, cfg *config.Service, gl *gitlab.Client, repos *repo.Manager, logger *slog.Logger) crossrepo.ContextRepos {
	c := cfg.Review.Context
	if !cfg.ContextEnabled() {
		if c.Enabled {
			logger.Info("context repositories off: gitlab.groups is empty")
		}
		return nil
	}
	p := contextrepos.New(contextrepos.Options{
		Lister:          gl, // the bot's own Reporter token: GET only
		Snapshots:       repos,
		Groups:          cfg.GitLab.Groups,
		RefreshInterval: c.RefreshInterval.D(),
		MaxRepos:        c.MaxRepos,
		MaxRepoBytes:    c.MaxRepoBytes(),
		Logger:          logger,
		Exclude:         c.Excluded,
		MainBranch:      cfg.GitLab.MainBranchFor, // gitlab.main_branch / main_branches
	})
	logger.Info("context repositories enabled", "groups", cfg.GitLab.Groups,
		"refresh_interval", c.RefreshInterval.D(), "max_repos", c.MaxRepos, "max_repo_mb", c.MaxRepoMB)
	go p.Run(ctx)
	return p
}
