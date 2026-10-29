package config

import (
	"os"
	"strings"
)

// Metrics configures the Prometheus endpoint GET /metrics.
type Metrics struct {
	// Enabled turns /metrics on (default true when unset); false -> 404.
	Enabled *bool `yaml:"enabled"`
	// Token (env METRICS_TOKEN preferred), when set, is required as
	// "Authorization: Bearer <token>" (no query token or cookie: the
	// endpoint is for scrapers). Empty = open; metrics carry no user,
	// project or MR labels.
	Token string `yaml:"token"`
}

// On reports whether /metrics is enabled.
func (m Metrics) On() bool { return defaultTrue(m.Enabled) }

func applyMetricsEnv(m *Metrics) {
	if v := os.Getenv("METRICS_TOKEN"); v != "" {
		m.Token = v
	}
	m.Token = strings.TrimSpace(m.Token)
}
