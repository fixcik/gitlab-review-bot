package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLearnDefaults(t *testing.T) {
	d := Defaults()
	l := d.Review.Learn
	if !l.Enabled || l.Model != "" || l.MaxPerHour != 30 || l.MaxInPrompt != 80 {
		t.Fatalf("learn defaults %+v", l)
	}
	if d.Claude.LightModel != "haiku" || d.Claude.Model != "sonnet" || d.Claude.AdvisorModel != "opus" || d.Claude.SearchModel != "haiku" {
		t.Fatalf("claude defaults %+v", d.Claude)
	}
	if d.ExtractModel() != "haiku" {
		t.Fatalf("extract model %q", d.ExtractModel())
	}
	d.Review.Learn.Model = "claude-haiku-4-5"
	if d.ExtractModel() != "claude-haiku-4-5" {
		t.Fatalf("override %q", d.ExtractModel())
	}
	d.Review.Learn.Model, d.Claude.LightModel = "", ""
	if d.ExtractModel() != "" {
		t.Fatalf("both empty -> CLI default, got %q", d.ExtractModel())
	}
}

func TestLearnFromYAML(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("gitlab:\n  url: https://g.example\nclaude:\n  model: opus\n  light_model: \"\"\nreview:\n  learn:\n    enabled: false\n    model: sonnet\n    max_per_hour: 5\n"), 0o600)
	cfg, err := LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.Learn.Enabled || cfg.Review.Learn.Model != "sonnet" || cfg.Review.Learn.MaxPerHour != 5 || cfg.Review.Learn.MaxInPrompt != 80 {
		t.Fatalf("learn %+v", cfg.Review.Learn)
	}
	if cfg.Claude.Model != "opus" || cfg.Claude.LightModel != "" || cfg.Review.AssignReviewer != true {
		t.Fatalf("claude %+v", cfg.Claude)
	}
}

func TestRepoLearnSwitch(t *testing.T) {
	svc := Defaults()
	if !Merge(&svc, nil).LearnEnabled {
		t.Fatal("default on")
	}
	off, err := ParseRepo([]byte("learn:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Merge(&svc, off).LearnEnabled {
		t.Fatal("repo switch ignored")
	}
	on, _ := ParseRepo([]byte("learn: {}\n"))
	if !Merge(&svc, on).LearnEnabled {
		t.Fatal("empty learn section must keep it on")
	}
	svc.Review.Learn.Enabled = false
	if Merge(&svc, on).LearnEnabled {
		t.Fatal("service switch ignored")
	}
}
