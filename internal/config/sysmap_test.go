package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSysmapDefaults(t *testing.T) {
	t.Setenv("MAP_TOKEN", "")
	c := Defaults()
	s := c.Review.Sysmap
	if !s.Enabled || s.MaxRunsPerHour != 20 || s.MaxFilesPerRepo != 400 || s.MaxFileBytes() != 64<<10 || s.RefreshInterval.D() != 10*time.Minute {
		t.Fatalf("defaults: %+v", s)
	}
	if c.SysmapEnabled() {
		t.Error("without gitlab.groups the map must be off")
	}
	c.GitLab.Groups = []string{"g"}
	if !c.SysmapEnabled() {
		t.Error("map must be on with groups")
	}
	c.Review.Context.Enabled = false
	if c.SysmapEnabled() {
		t.Error("map needs context repositories")
	}
	if c.SysmapModel() != "sonnet" {
		t.Errorf("default model = %q", c.SysmapModel())
	}
	c.Review.Sysmap.Model = ""
	if c.SysmapModel() != "haiku" {
		t.Errorf("empty model must fall back to claude.light_model, got %q", c.SysmapModel())
	}
	if s.AccessToken("u") != "u" || s.AccessToken("") != "" {
		t.Error("token fallback")
	}
	s.Token = " f "
	if s.AccessToken("u") != "f" {
		t.Error("file token")
	}
	t.Setenv("MAP_TOKEN", "env")
	if s.AccessToken("u") != "env" {
		t.Error("env token")
	}
}

func TestSysmapFromFileAndValidation(t *testing.T) {
	t.Setenv("GITLAB_URL", "https://g.example")
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte(`review:
  sysmap:
    model: haiku
    max_runs_per_hour: 0
    max_files_per_repo: 10
    max_file_kb: 32
    refresh_interval: 30m
    exclude: ["legacy/**"]
    hints:
      ORDERS_URL: backend/orders
      orders.internal: /backend/orders/
`), 0o600)
	cfg, err := LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Review.Sysmap
	if s.MaxRunsPerHour != 0 || s.MaxFilesPerRepo != 10 || s.MaxFileBytes() != 32<<10 || s.RefreshInterval.D() != 30*time.Minute || len(s.Hints) != 2 {
		t.Fatalf("parsed: %+v", s)
	}
	if !s.Excluded("Legacy/Old") || s.Excluded("backend/orders") {
		t.Error("exclude")
	}

	bad := Defaults()
	bad.Review.Sysmap.MaxRunsPerHour = -1
	bad.Review.Sysmap.MaxFilesPerRepo = 0
	bad.Review.Sysmap.MaxFileKB = 4096
	bad.Review.Sysmap.RefreshInterval = Duration(time.Second)
	bad.Review.Sysmap.Exclude = []string{"[bad"}
	bad.Review.Sysmap.Hints = map[string]string{"X": "not a path", " ": "g/p"}
	errs := strings.Join(bad.Review.Sysmap.validate(), "\n")
	for _, want := range []string{"max_runs_per_hour", "max_files_per_repo", "max_file_kb", "refresh_interval", "exclude", "not a full project path", "bad key"} {
		if !strings.Contains(errs, want) {
			t.Errorf("validation misses %q in:\n%s", want, errs)
		}
	}
	bad.Review.Sysmap.Enabled = false
	if len(bad.Review.Sysmap.validate()) != 0 {
		t.Error("a disabled map is not validated")
	}
}

func TestRepoSysmapExclude(t *testing.T) {
	r, err := ParseRepo([]byte("sysmap:\n  exclude: [\" /legacy/** \", \"\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sysmap.Exclude) != 1 || r.Sysmap.Exclude[0] != "legacy/**" {
		t.Fatalf("exclude = %v", r.Sysmap.Exclude)
	}
	if _, err := ParseRepo([]byte("sysmap:\n  exclude: [\"[bad\"]\n")); err == nil {
		t.Error("bad glob accepted")
	}
}
