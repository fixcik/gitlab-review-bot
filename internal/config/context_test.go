package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewContextDefaults(t *testing.T) {
	c := Defaults()
	rc := c.Review.Context
	if !rc.Enabled || rc.RefreshInterval.D() != 10*time.Minute || rc.MaxRepos != 50 || rc.MaxRepoMB != 500 {
		t.Errorf("defaults %+v", rc)
	}
	if rc.MaxRepoBytes() != 500<<20 {
		t.Errorf("bytes %d", rc.MaxRepoBytes())
	}
	// No groups: off.
	if c.ContextEnabled() {
		t.Error("context must be off without gitlab.groups")
	}
	c.GitLab.Groups = []string{" / "}
	if c.ContextEnabled() {
		t.Error("blank groups do not count")
	}
	c.GitLab.Groups = []string{"backend"}
	if !c.ContextEnabled() {
		t.Error("context must be on with groups")
	}
	c.Review.Context.Enabled = false
	if c.ContextEnabled() {
		t.Error("enabled=false must turn it off")
	}
}

func TestReviewContextFromFileAndValidation(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := LoadService(write("gitlab:\n  url: https://g.example\n  groups: [backend]\nreview:\n  context:\n    refresh_interval: 30m\n    max_repos: 7\n"))
	if err != nil {
		t.Fatal(err)
	}
	rc := cfg.Review.Context
	if !rc.Enabled || rc.RefreshInterval.D() != 30*time.Minute || rc.MaxRepos != 7 || rc.MaxRepoMB != 500 {
		t.Errorf("parsed %+v", rc)
	}
	if !cfg.ContextEnabled() {
		t.Error("should be enabled")
	}
	for body, want := range map[string]string{
		"review:\n  context:\n    refresh_interval: 10s\n": "refresh_interval",
		"review:\n  context:\n    max_repos: 0\n":          "max_repos",
		"review:\n  context:\n    max_repo_mb: -1\n":       "max_repo_mb",
	} {
		_, err := LoadService(write("gitlab:\n  url: https://g.example\n" + body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v", body, err)
		}
	}
	// Disabled: limits are not validated.
	if _, err := LoadService(write("gitlab:\n  url: https://g.example\nreview:\n  context:\n    enabled: false\n    max_repos: 0\n")); err != nil {
		t.Errorf("disabled context must not be validated: %v", err)
	}
}

func TestRepoContext(t *testing.T) {
	svc := Defaults()
	e := Merge(&svc, nil)
	if !e.ContextEnabled() || e.ContextExcluded("a/b") {
		t.Error("defaults: enabled, nothing excluded")
	}
	r, err := ParseRepo([]byte("context:\n  exclude: [\" legacy/** \", \"Backend/Huge\", \"\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	e = Merge(&svc, r)
	if !e.ContextEnabled() {
		t.Error("enabled by default")
	}
	for p, want := range map[string]bool{
		"legacy/a": true, "legacy/sub/b": true, "backend/huge": true, "backend/api": false, "legacyx/a": false,
	} {
		if e.ContextExcluded(p) != want {
			t.Errorf("excluded(%q) != %v", p, want)
		}
	}
	r, err = ParseRepo([]byte("context:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Merge(&svc, r).ContextEnabled() {
		t.Error("enabled: false must disable")
	}
	if _, err := ParseRepo([]byte("context:\n  exclude: [\"a/[b\"]\n")); err == nil {
		t.Error("invalid glob must be rejected")
	}
}
