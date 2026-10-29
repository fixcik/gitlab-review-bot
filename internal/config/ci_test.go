package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCIDefaults(t *testing.T) {
	d := Defaults()
	c := d.Review.CI
	if !c.Enabled || !c.Explain || !c.MainBranch || c.MaxLogLines != 40 || c.MaxTests != 10 || c.Model != "" ||
		len(c.IgnoreJobs) != 0 || len(c.NotifyMain) != 0 || strings.Join(c.MainSources, ",") != "push" {
		t.Errorf("defaults %+v", c)
	}
	if errs := c.validate(); len(errs) > 0 {
		t.Errorf("defaults invalid: %v", errs)
	}
	// Model: review.ci.model, else claude.light_model, else claude.model.
	if m := d.CIExplainModel(); m != "haiku" {
		t.Errorf("default model %q", m)
	}
	d.Claude.LightModel = ""
	if m := d.CIExplainModel(); m != "sonnet" {
		t.Errorf("no light model: %q", m)
	}
	d.Review.CI.Model = "opus"
	if m := d.CIExplainModel(); m != "opus" {
		t.Errorf("explicit: %q", m)
	}
}

func TestCIValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*CI)
		want string
	}{
		{"lines low", func(c *CI) { c.MaxLogLines = 2 }, "review.ci.max_log_lines"},
		{"lines high", func(c *CI) { c.MaxLogLines = 1000 }, "review.ci.max_log_lines"},
		{"tests", func(c *CI) { c.MaxTests = 0 }, "review.ci.max_tests"},
		{"tests high", func(c *CI) { c.MaxTests = 51 }, "review.ci.max_tests"},
		{"glob", func(c *CI) { c.IgnoreJobs = []string{"deploy-["} }, `invalid glob "deploy-["`},
		{"empty glob", func(c *CI) { c.IgnoreJobs = []string{" "} }, "empty glob"},
		{"user", func(c *CI) { c.NotifyMain = []string{"@ok", "bad name"} }, `"bad name" is not a GitLab username`},
		{"model", func(c *CI) { c.Model = "a b" }, "review.ci.model"},
		{"child source", func(c *CI) { c.MainSources = []string{"push", "parent_pipeline"} }, "parent_pipeline (child pipelines) never counts"},
		{"unknown source", func(c *CI) { c.MainSources = []string{"nightly"} }, `unknown pipeline source "nightly"`},
	} {
		c := defaultCI()
		tc.edit(&c)
		if errs := strings.Join(c.validate(), "; "); !strings.Contains(errs, tc.want) {
			t.Errorf("%s: errors %q, want %q", tc.name, errs, tc.want)
		}
	}
	c := defaultCI()
	c.NotifyMain = []string{"@alice", " bob "}
	if errs := c.validate(); len(errs) > 0 || strings.Join(c.NotifyUsers(), ",") != "alice,bob" {
		t.Errorf("%v %v", errs, c.NotifyUsers())
	}
}

// The service config is validated by LoadService; a repository may turn the
// digest off and add ignore globs, never turn it on.
func TestCILoadAndMerge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("review:\n  ci:\n    explain: false\n    ignore_jobs: [\"deploy-*\"]\n    notify_main: [lead]\n    max_tests: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := readService(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Review.CI.Explain || !c.Review.CI.Enabled || c.Review.CI.MaxTests != 5 || c.Review.CI.MaxLogLines != 40 {
		t.Fatalf("%+v", c.Review.CI)
	}
	r, err := ParseRepo([]byte("ci:\n  ignore_jobs: [\"pages\", \" \"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(c, r)
	if !e.CI.Enabled || e.CI.Explain || strings.Join(e.CI.IgnoreJobs, ",") != "deploy-*,pages" || e.CI.MaxTests != 5 {
		t.Fatalf("%+v", e.CI)
	}
	if !e.CI.IgnoredJob("deploy-prod") || !e.CI.IgnoredJob("pages") || e.CI.IgnoredJob("tests") {
		t.Error("IgnoredJob")
	}
	r, err = ParseRepo([]byte("ci:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Merge(c, r).CI.Enabled {
		t.Error("repo must be able to turn it off")
	}
	c.Review.CI.Enabled = false
	r, _ = ParseRepo([]byte("ci:\n  enabled: true\n"))
	if Merge(c, r).CI.Enabled {
		t.Error("repo must not turn it on")
	}
	if _, err := ParseRepo([]byte("ci:\n  ignore_jobs: [\"[\"]\n")); err == nil || !strings.Contains(err.Error(), "ci.ignore_jobs") {
		t.Errorf("bad repo glob: %v", err)
	}
	if Merge(c, nil).CI.Enabled {
		t.Error("nil repo")
	}
}

func TestCIMainSourceAllowed(t *testing.T) {
	c := defaultCI()
	for src, want := range map[string]bool{"push": true, "": true, "schedule": false, "web": false, "api": false,
		"trigger": false, "parent_pipeline": false} {
		if got := c.MainSourceAllowed(src); got != want {
			t.Errorf("default %q: %v", src, got)
		}
	}
	c.MainSources = nil // empty = the default
	if !c.MainSourceAllowed("push") || c.MainSourceAllowed("schedule") {
		t.Error("empty main_sources must mean the default")
	}
	c.MainSources = []string{"push", "schedule", "parent_pipeline"}
	if !c.MainSourceAllowed("schedule") || c.MainSourceAllowed("parent_pipeline") {
		t.Error("listed / child")
	}
	if errs := (CI{MaxLogLines: 40, MaxTests: 10, MainSources: []string{"push", "web", "schedule", "api", "trigger"}}).validate(); len(errs) > 0 {
		t.Errorf("valid sources refused: %v", errs)
	}
}
