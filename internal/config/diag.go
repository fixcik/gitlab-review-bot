package config

import (
	"fmt"
	"os"
	"time"
)

// Diag configures the diagnostics of recent runs (/runs, `reviewbot runs`):
// for every handled job the bot keeps its timeline and the full Claude
// sessions (prompts, transcript, output) under <data_dir>/diag.
type Diag struct {
	// Enabled turns recording and the /runs pages on (default true).
	Enabled *bool `yaml:"enabled"`
	// Retention is how long run records are kept. Default 48h.
	Retention Duration `yaml:"retention"`
	// MaxRunMB caps one run on disk; longer transcripts are cut with a
	// marker. Default 20.
	MaxRunMB int `yaml:"max_run_mb"`
	// MaxTotalMB caps all runs; the oldest are evicted first. Default 2048.
	MaxTotalMB int `yaml:"max_total_mb"`
	// Token (env DIAG_TOKEN preferred) protects /runs like usage.token
	// protects /usage. Empty = usage.token / USAGE_TOKEN is used; when that
	// is empty too, the pages are open.
	Token string `yaml:"token"`
}

// Diag defaults.
const (
	DefaultDiagRetention  = 48 * time.Hour
	DefaultDiagMaxRunMB   = 20
	DefaultDiagMaxTotalMB = 2048
)

// On reports whether diagnostics are enabled.
func (d Diag) On() bool { return defaultTrue(d.Enabled) }

// RetentionD returns the retention (default 48h).
func (d Diag) RetentionD() time.Duration { return positiveOr(d.Retention.D(), DefaultDiagRetention) }

// MaxRunBytes returns max_run_mb in bytes (default 20 MB).
func (d Diag) MaxRunBytes() int64 { return int64(positiveOr(d.MaxRunMB, DefaultDiagMaxRunMB)) << 20 }

// MaxTotalBytes returns max_total_mb in bytes (default 2048 MB).
func (d Diag) MaxTotalBytes() int64 {
	return int64(positiveOr(d.MaxTotalMB, DefaultDiagMaxTotalMB)) << 20
}

// AccessToken is the token protecting /runs: DIAG_TOKEN, else diag.token,
// else the usage page token (usage.token / USAGE_TOKEN, already resolved in
// usageToken). Empty = open.
func (d Diag) AccessToken(usageToken string) string {
	return pageAccessToken("DIAG_TOKEN", d.Token, usageToken)
}

// LoadDiag reads only data_dir and the diag section of a service config
// (defaults applied, no secret validation). Used by `reviewbot runs`.
func LoadDiag(path string) (dataDir string, d Diag, err error) {
	cfg := Defaults()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", Diag{}, err
		}
		if err := decodeOver(b, &cfg); err != nil {
			return "", Diag{}, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return cfg.DataDir, cfg.Diag, nil
}
