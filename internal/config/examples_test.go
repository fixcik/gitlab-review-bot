package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// The example files shipped in the repository root must parse with the real
// loaders and must not contain keys the structs do not know (a typo or a
// renamed key in an example silently does nothing for whoever copies it).
func TestExampleConfigsParse(t *testing.T) {
	root := filepath.Join("..", "..")

	t.Run("config.example.yaml", func(t *testing.T) {
		path := filepath.Join(root, "config.example.yaml")
		for _, k := range []string{"GITLAB_URL", "GITLAB_HOOKS_TOKEN", "GITLAB_SERVICE_TOKEN", "JIRA_URL", "JIRA_TOKEN", "USAGE_TOKEN", "DIAG_TOKEN", "MAP_TOKEN"} {
			t.Setenv(k, "")
		}
		t.Setenv("GITLAB_TOKEN", "glpat-test")
		t.Setenv("GITLAB_WEBHOOK_SECRET", "secret")
		cfg, err := LoadService(path)
		if err != nil {
			t.Fatalf("LoadService: %v", err)
		}
		if cfg.GitLab.URL == "" || cfg.Review.Lint.Timeout.D() <= 0 || !cfg.Review.Memory.On() || !cfg.Diag.On() {
			t.Fatalf("example not applied: %+v", cfg.Review)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := Defaults()
		if err := yaml.UnmarshalWithOptions(b, &s, yaml.DisallowUnknownField()); err != nil {
			t.Fatalf("unknown or invalid key in config.example.yaml: %v", err)
		}
	})

	t.Run(".reviewbot.example.yaml", func(t *testing.T) {
		b, err := os.ReadFile(filepath.Join(root, ".reviewbot.example.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		r, err := ParseRepo(b)
		if err != nil {
			t.Fatalf("ParseRepo: %v", err)
		}
		if len(r.PathInstructions) == 0 || r.Compat == nil || r.Lint == nil || r.Memory == nil {
			t.Fatalf("example not applied: %+v", r)
		}
		var strict Repo
		if err := yaml.UnmarshalWithOptions(b, &strict, yaml.DisallowUnknownField()); err != nil {
			t.Fatalf("unknown or invalid key in .reviewbot.example.yaml: %v", err)
		}
		def := Defaults()
		eff := Merge(&def, r)
		if !eff.Enabled || eff.MaxFiles != 50 || len(eff.PathInstructions) != 2 {
			t.Fatalf("merge: %+v", eff)
		}
	})
}

// The local test stand (e2e/local) ships a service config and one
// .reviewbot.yaml per test project: they must load with the real loaders and
// have no unknown keys, so the stand does not silently drift from the code.
func TestLocalStandConfigs(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(root, "e2e", "local")

	t.Run("config.yaml", func(t *testing.T) {
		// Relative paths (data_dir, review.skills.dir) are relative to the
		// repository root, where the bot is started.
		t.Chdir(root)
		for _, k := range []string{"GITLAB_URL", "GITLAB_HOOKS_TOKEN", "GITLAB_SERVICE_TOKEN", "GITLAB_WEBHOOK_SECRET", "JIRA_URL", "JIRA_TOKEN", "USAGE_TOKEN", "DIAG_TOKEN", "MAP_TOKEN", "METRICS_TOKEN"} {
			t.Setenv(k, "")
		}
		t.Setenv("GITLAB_TOKEN", "glpat-test")
		path := filepath.Join("e2e", "local", "config.yaml")
		cfg, err := LoadService(path)
		if err != nil {
			t.Fatalf("LoadService: %v", err)
		}
		if cfg.GitLab.URL != "http://gitlab:8929" || cfg.GitLab.Events != EventsPoll ||
			len(cfg.GitLab.Groups) != 1 || cfg.GitLab.Groups[0] != "reviewbot-sandbox" ||
			!cfg.Review.Skills.On() || cfg.Jira.Enabled() || !cfg.ContextEnabled() {
			t.Fatalf("unexpected local config: gitlab=%+v skills=%+v", cfg.GitLab, cfg.Review.Skills)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := Defaults()
		if err := yaml.UnmarshalWithOptions(b, &s, yaml.DisallowUnknownField()); err != nil {
			t.Fatalf("unknown or invalid key in e2e/local/config.yaml: %v", err)
		}
	})

	t.Run("fixtures .reviewbot.yaml", func(t *testing.T) {
		var files []string
		err := filepath.WalkDir(filepath.Join(local, "fixtures"), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && d.Name() == ".reviewbot.yaml" {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) < 10 {
			t.Fatalf("expected the .reviewbot.yaml of every test project, found %d", len(files))
		}
		// Opt-in (review.require_repo_config): every reviewed rb-* project
		// has the file on main; rb-noconfig (R6-R9) and rb-big (context only)
		// on purpose do not.
		projects, err := filepath.Glob(filepath.Join(local, "fixtures", "rb-*"))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range projects {
			name := filepath.Base(d)
			_, err := os.Stat(filepath.Join(d, "base", ".reviewbot.yaml"))
			switch without := name == "rb-noconfig" || name == "rb-big"; {
			case without && err == nil:
				t.Errorf("%s must not have .reviewbot.yaml on main", name)
			case !without && err != nil:
				t.Errorf("%s: no base/.reviewbot.yaml: the bot would not review it (opt-in)", name)
			}
		}
		for _, p := range files {
			rel, _ := filepath.Rel(local, p)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			// An MR that brings an invalid config on purpose (R8) says so
			// in its .mr.json: it must stay invalid.
			if ctl, err := os.ReadFile(filepath.Join(filepath.Dir(p), ".mr.json")); err == nil &&
				strings.Contains(string(ctl), `"invalid_repo_config": true`) {
				if _, err := ParseRepo(b); err == nil {
					t.Errorf("%s: must be invalid (invalid_repo_config in .mr.json)", rel)
				}
				continue
			}
			if _, err := ParseRepo(b); err != nil {
				t.Errorf("%s: ParseRepo: %v", rel, err)
				continue
			}
			var strict Repo
			if err := yaml.UnmarshalWithOptions(b, &strict, yaml.DisallowUnknownField()); err != nil {
				t.Errorf("%s: unknown or invalid key: %v", rel, err)
			}
		}
	})
}
