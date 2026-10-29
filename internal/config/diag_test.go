package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiagDefaults(t *testing.T) {
	t.Setenv("GITLAB_URL", "https://g.example")
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("USAGE_TOKEN", "")
	t.Setenv("DIAG_TOKEN", "")
	cfg, err := LoadService("")
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Diag
	if !d.On() || d.RetentionD() != 48*time.Hour || d.MaxRunBytes() != 20<<20 || d.MaxTotalBytes() != 2048<<20 || d.AccessToken("") != "" {
		t.Fatalf("defaults: %+v", d)
	}
	if d.AccessToken("usage-tok") != "usage-tok" {
		t.Fatal("no fallback to the usage token")
	}
}

func TestDiagFromFileAndEnv(t *testing.T) {
	t.Setenv("DIAG_TOKEN", "")
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("data_dir: /srv/bot\ndiag:\n  enabled: false\n  retention: 24h\n  max_run_mb: 5\n  max_total_mb: 100\n  token: \" file-tok \"\n"), 0o600)
	dir, d, err := LoadDiag(p)
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/srv/bot" || d.On() || d.RetentionD() != 24*time.Hour || d.MaxRunBytes() != 5<<20 || d.MaxTotalBytes() != 100<<20 || d.AccessToken("u") != "file-tok" {
		t.Fatalf("file: %s %+v", dir, d)
	}
	t.Setenv("DIAG_TOKEN", "env-tok")
	if d.AccessToken("u") != "env-tok" {
		t.Fatal("env override")
	}
}
