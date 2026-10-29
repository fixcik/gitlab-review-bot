package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryDefaultsAndSwitches(t *testing.T) {
	svc := Defaults()
	m := svc.Review.Memory
	if !m.On() || m.TotalCap() != 6000 || m.SectionCap() != 1500 {
		t.Fatalf("defaults %+v", m)
	}
	if !Merge(&svc, nil).MemoryEnabled {
		t.Fatal("default on")
	}
	off, err := ParseRepo([]byte("memory:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Merge(&svc, off).MemoryEnabled {
		t.Fatal("repo switch ignored")
	}
	on, _ := ParseRepo([]byte("memory: {}\n"))
	if !Merge(&svc, on).MemoryEnabled {
		t.Fatal("empty section keeps it on")
	}
	f := false
	svc.Review.Memory.Enabled = &f
	if Merge(&svc, on).MemoryEnabled {
		t.Fatal("service switch ignored")
	}
	if (Memory{MaxChars: 1000}).SectionCap() != 1000 {
		t.Fatal("section cap must not exceed the total")
	}
}

func TestMemoryFromYAML(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("gitlab:\n  url: https://g.example\nreview:\n  memory:\n    enabled: false\n    max_chars: 3000\n"), 0o600)
	cfg, err := LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.Memory.On() || cfg.Review.Memory.TotalCap() != 3000 {
		t.Fatalf("memory %+v", cfg.Review.Memory)
	}
}
