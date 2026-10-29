package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/diagweb"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// startDiag creates the diagnostics recorder (nil when diag.enabled is
// false or it cannot start; the bot then runs without it) and starts its
// retention loop.
func startDiag(ctx context.Context, cfg *config.Service, dataDir string, st *store.SQLite, logger *slog.Logger) *diag.Recorder {
	if !cfg.Diag.On() {
		logger.Info("diagnostics disabled (diag.enabled: false)")
		return nil
	}
	rec, err := diag.New(diag.Options{
		Root: filepath.Join(dataDir, "diag"), Index: st,
		Retention: cfg.Diag.RetentionD(), MaxRunBytes: cfg.Diag.MaxRunBytes(), MaxTotalBytes: cfg.Diag.MaxTotalBytes(),
		Secrets:      diagSecrets(cfg),
		MaxDiffBytes: cfg.Review.MaxDiffBytes,
		GitLabURL:    cfg.GitLab.URL,
		Logger:       logger,
	})
	if err != nil {
		logger.Error("diagnostics disabled: cannot start", "err", err)
		return nil
	}
	go rec.Run(ctx)
	logger.Info("diagnostics enabled", "dir", rec.Root(), "retention", cfg.Diag.RetentionD())
	return rec
}

// diagSecrets are the literal secret values scrubbed from every stored
// string, on top of the pattern-based scrubbing.
func diagSecrets(cfg *config.Service) []string {
	s := []string{cfg.GitLab.Token, cfg.GitLab.WebhookSecret, cfg.GitLab.ServiceToken, cfg.Jira.Token, cfg.Usage.Token,
		cfg.Diag.AccessToken(cfg.Usage.Token), cfg.Metrics.Token, cfg.Review.Sysmap.AccessToken(cfg.Usage.Token)}
	// Trace exporter credentials (tracing.headers and the variables they
	// reference).
	s = append(s, cfg.Tracing.HeaderValues()...)
	for _, name := range cfg.Tracing.HeaderEnv() {
		s = append(s, os.Getenv(name))
	}
	for _, name := range core.SecretEnv() {
		s = append(s, os.Getenv(name))
	}
	var out []string
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// registerDiagWeb mounts /runs (404 when diagnostics are off); st feeds its
// read-only queue strip.
func registerDiagWeb(mux *http.ServeMux, cfg *config.Service, rec *diag.Recorder, st *store.SQLite, nav webui.Nav, logger *slog.Logger) {
	o := diagweb.Options{Nav: nav,
		Reader: rec.Reader(), Enabled: rec != nil, Token: cfg.Diag.AccessToken(cfg.Usage.Token),
		Retention: cfg.Diag.RetentionD(), Now: time.Now, Logger: logger, TraceURL: cfg.Tracing.UIURL,
	}
	if st != nil {
		o.Queue = st
	}
	diagweb.Register(mux, o)
}
