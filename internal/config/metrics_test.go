package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMetricsConfig(t *testing.T) {
	t.Setenv("GITLAB_URL", "https://g.example")
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("METRICS_TOKEN", "")
	cfg, err := LoadService("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Metrics.On() || cfg.Metrics.Token != "" {
		t.Fatalf("defaults: %+v", cfg.Metrics)
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("metrics:\n  enabled: false\n  token: \" file-tok \"\n"), 0o600)
	if cfg, err = LoadService(p); err != nil {
		t.Fatal(err)
	}
	if cfg.Metrics.On() || cfg.Metrics.Token != "file-tok" {
		t.Fatalf("file: %+v", cfg.Metrics)
	}
	t.Setenv("METRICS_TOKEN", "env-tok")
	if cfg, err = LoadService(p); err != nil || cfg.Metrics.Token != "env-tok" {
		t.Fatalf("env override: %+v %v", cfg.Metrics, err)
	}
}
