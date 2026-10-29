package main

import (
	"log/slog"
	"net/http"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

// newTelemetry creates the metrics (nil when metrics.enabled is false: every
// decorator is then a no-op and /metrics answers 404).
func newTelemetry(cfg *config.Service) *telemetry.Metrics {
	if !cfg.Metrics.On() {
		return nil
	}
	return telemetry.New(version)
}

// observeGitLab counts every HTTP attempt of the clients (nil-safe).
func observeGitLab(m *telemetry.Metrics, clients ...*gitlab.Client) {
	if m == nil {
		return
	}
	for _, c := range clients {
		if c != nil {
			c.SetObserver(m.GitLabObserver())
		}
	}
}

// registerTelemetry mounts /metrics (404 when disabled) and /readyz (always:
// it is the readiness probe, independent of metrics.enabled).
func registerTelemetry(mux *http.ServeMux, m *telemetry.Metrics, cfg *config.Service, gl *gitlab.Client, st *store.SQLite, logger *slog.Logger) {
	m.RegisterMetrics(mux, telemetry.MetricsOptions{Enabled: cfg.Metrics.On(), Token: cfg.Metrics.Token, Logger: logger})
	checks := []telemetry.Check{telemetry.GitLabCheck(gl.CurrentUser), telemetry.StoreCheck(st.Ping)}
	if cfg.Sandbox.Mode == "docker" {
		checks = append(checks, telemetry.DockerCheck("docker", nil))
	}
	mux.Handle(telemetry.ReadyPath, m.NewReadiness(telemetry.ReadinessOptions{
		Checks: checks, Secrets: diagSecrets(cfg),
	}))
}
