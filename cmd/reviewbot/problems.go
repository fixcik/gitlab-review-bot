package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/knowledgeweb"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/problemsweb"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// newProblems builds the verification problems service (review.problems;
// nil when off: the handler wrapper and every hook are no-ops then).
func newProblems(cfg *config.Service, st *store.SQLite, tm *telemetry.Metrics, logger *slog.Logger) *problems.Service {
	p := cfg.Review.Problems
	if !p.Enabled {
		logger.Info("verification problems off (review.problems.enabled: false)")
		return nil
	}
	svc := problems.New(problems.Options{
		Store: st, Notify: p.NotifyUsers(), NotifyMode: p.Mode(), Renotify: p.RenotifyD(),
		PageURL: p.PageURL, Retention: p.RetentionD(),
		Scrub: diag.ScrubFunc(diagSecrets(cfg)),
		// The job's diagnostics run: the problems wrapper runs inside
		// diag.Recorder.Handler, so the run is already in the context.
		RunID: func(ctx context.Context) string { return diag.FromContext(ctx).ID() },
		OnRecord: func(pr problems.Problem, fresh bool) {
			tm.ObserveProblem(string(pr.Kind), string(pr.Source), fresh)
		},
		Logger: logger,
	})
	logger.Info("verification problems enabled", "notify", len(p.NotifyUsers()), "notify_mode", p.Mode(),
		"renotify_after", p.RenotifyD(), "page_url", p.PageURL)
	return svc
}

// sysmapProblems turns scan errors of the system map (background work, no
// job) into problems of the scanned repository, next to the metric.
func sysmapProblems(svc *problems.Service, next func(repo, status string)) func(repo, status string) {
	return func(repo, status string) {
		if next != nil {
			next(repo, status)
		}
		if status == sysmap.StatusError {
			svc.Record(context.Background(), problems.Problem{Kind: problems.SysmapFailed, Source: problems.SourceBot,
				ProjectPath: repo, Subject: "карта системы: сканирование репозитория не удалось", Detail: "подробности — на /map и в логах бота"})
		}
	}
}

// registerProblemsWeb mounts /problems (404 when problems are off). It uses
// the /usage token.
func registerProblemsWeb(mux *http.ServeMux, cfg *config.Service, st *store.SQLite, svc *problems.Service, nav webui.Nav, logger *slog.Logger) {
	problemsweb.Register(mux, problemsweb.Options{
		Nav: nav, Store: st, Enabled: svc != nil, Token: cfg.Usage.Token, GitLabURL: cfg.GitLab.URL, Now: time.Now, Logger: logger,
	})
}

// registerKnowledgeWeb mounts /knowledge: learned rules, project memory and
// finding accuracy. It uses the /usage token and is always on (the data are
// the bot's own; with nothing learned yet the blocks are empty).
func registerKnowledgeWeb(mux *http.ServeMux, cfg *config.Service, st *store.SQLite, nav webui.Nav, logger *slog.Logger) {
	knowledgeweb.Register(mux, knowledgeweb.Options{
		Nav: nav, Store: st, Enabled: true, Token: cfg.Usage.Token, GitLabURL: cfg.GitLab.URL, Now: time.Now, Logger: logger,
	})
}
