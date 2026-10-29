package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/hooksync"
)

// newHookSyncer builds a hooksync.Syncer from the service config.
func newHookSyncer(cfg *config.Service, gl *gitlab.Client, dryRun bool, logger *slog.Logger) *hooksync.Syncer {
	return hooksync.New(hooksync.Options{
		API:       gl,
		Groups:    cfg.GitLab.Groups,
		URL:       cfg.GitLab.WebhookURL,
		Secret:    cfg.GitLab.WebhookSecret,
		SSLVerify: cfg.GitLab.HookSSLVerify,
		DryRun:    dryRun,
		Logger:    logger,
		Excluded:  cfg.GitLab.Excluded,
		// review.ci: finished pipelines feed the CI failure digest.
		PipelineEvents: cfg.Review.CI.Enabled,
	})
}

// newServiceClient builds the client for the service token (Maintainer). It
// may write ONLY project hooks, the MR reviewer list and thread resolution:
// no notes, no code, no merges, no settings.
func newServiceClient(cfg *config.Service, logger *slog.Logger) (*gitlab.Client, error) {
	if cfg.GitLab.ServiceToken == "" {
		return nil, fmt.Errorf("не задан GITLAB_SERVICE_TOKEN (отдельный токен Maintainer)")
	}
	return newGitLabClient(cfg, cfg.GitLab.ServiceToken, gitlab.ServiceWrites, logger)
}

// startGroupFeatures logs the groups setup and, when gitlab.sync_hooks is on,
// starts the periodic hook sync in the background. Called from serve.
func startGroupFeatures(ctx context.Context, cfg *config.Service, svc *gitlab.Client, logger *slog.Logger) error {
	if len(cfg.GitLab.Groups) == 0 {
		logger.Warn("gitlab.groups is empty: events from every project that has the bot's webhook are accepted")
	} else {
		logger.Info("reviewing projects of groups", "groups", cfg.GitLab.Groups)
	}
	if !cfg.GitLab.SyncHooks {
		return nil
	}
	if svc == nil {
		return fmt.Errorf("gitlab.sync_hooks: не задан GITLAB_SERVICE_TOKEN")
	}
	logger.Info("webhook sync enabled", "url", cfg.GitLab.WebhookURL, "interval", cfg.GitLab.SyncInterval.D())
	go newHookSyncer(cfg, svc, false, logger).Run(ctx, cfg.GitLab.SyncInterval.D())
	return nil
}

// runSyncHooks implements `reviewbot sync-hooks`: one sync pass with token
// reset, a human-readable report on stdout, an error if anything failed.
func runSyncHooks(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync-hooks", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml")
	dryRun := fs.Bool("dry-run", false, "only show what would be changed")
	fs.Parse(args)

	// Human-readable progress on stderr; the report goes to stdout.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))

	// Only what a hook sync needs: no GITLAB_TOKEN, sandbox or Jira checks.
	cfg, err := config.LoadSyncHooks(*cfgPath)
	if err != nil {
		return fmt.Errorf("sync-hooks: %w", err)
	}

	gl, err := newServiceClient(cfg, logger)
	if err != nil {
		return err
	}

	rep, err := newHookSyncer(cfg, gl, *dryRun, logger).Sync(ctx, true)
	printSyncReport(os.Stdout, cfg, *dryRun, rep)
	if err != nil {
		return err
	}
	if rep.Failed > 0 {
		return fmt.Errorf("sync-hooks: ошибок: %d", rep.Failed)
	}
	return nil
}

// skippedAdvice: hooks are managed with the service token, so it is the
// service token's user that needs Maintainer/Owner; the bot account itself
// stays Reporter.
const skippedAdvice = "Для пропущенных проектов выдайте пользователю служебного токена (GITLAB_SERVICE_TOKEN) роль Maintainer или Owner " +
	"(или настройте вебхук вручную). Бот-аккаунту (GITLAB_TOKEN) достаточно Reporter."

func printSyncReport(w io.Writer, cfg *config.Service, dryRun bool, rep hooksync.Report) {
	title := "Синхронизация вебхуков"
	created, updated := "создано", "обновлено"
	if dryRun {
		title += " (dry-run, изменения не вносились)"
		created, updated = "будет создано", "будет обновлено"
	}
	fmt.Fprintln(w, title)
	rows := []struct {
		label, value string
	}{
		{"URL", cfg.GitLab.WebhookURL},
		{"Группы", strings.Join(cfg.GitLab.Groups, ", ")},
		{"Проектов", strconv.Itoa(rep.Projects)},
		{created, strconv.Itoa(rep.Created)},
		{updated, strconv.Itoa(rep.Updated)},
		{"без изменений", strconv.Itoa(rep.Unchanged)},
		{"пропущено (нет прав)", strconv.Itoa(rep.Skipped)},
		{"исключено (gitlab.exclude)", strconv.Itoa(rep.Excluded)},
		{"ошибок", strconv.Itoa(rep.Failed)},
	}
	width := 0
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r.label))
	}
	for _, r := range rows {
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(r.label))
		fmt.Fprintf(w, "  %s:%s %s\n", r.label, pad, r.value)
	}
	if len(rep.Errors) > 0 {
		fmt.Fprintln(w, "Проблемы:")
		for _, e := range rep.Errors {
			fmt.Fprintf(w, "  - %s\n", e)
		}
	}
	if rep.Skipped > 0 {
		fmt.Fprintln(w, skippedAdvice)
	}
}
