package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoPathInstructionsParse(t *testing.T) {
	r, err := ParseRepo([]byte(`
path_instructions:
  - path: "migrations/**"
    instructions: "Проверяй обратимость и блокировки таблиц"
  - path: " ./api/**/*.go "
    instructions: |
      Проверяй валидацию входных данных.
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []PathInstruction{
		{Path: "migrations/**", Instructions: "Проверяй обратимость и блокировки таблиц"},
		{Path: "api/**/*.go", Instructions: "Проверяй валидацию входных данных."},
	}
	if fmt.Sprint(r.PathInstructions) != fmt.Sprint(want) {
		t.Fatalf("got %+v", r.PathInstructions)
	}
	if !r.PathInstructions[0].MatchPath("migrations/2026/001_init.sql") || r.PathInstructions[0].MatchPath("cmd/migrations.go") {
		t.Fatal("glob matching")
	}
	if !r.PathInstructions[1].MatchPath("api/v1/h.go") || r.PathInstructions[1].MatchPath("api/v1/h.ts") {
		t.Fatal("glob matching api")
	}
}

func TestRepoPathInstructionsValidation(t *testing.T) {
	long := strings.Repeat("я", MaxPathInstructionRunes+1)
	var many strings.Builder
	many.WriteString("path_instructions:\n")
	for i := 0; i <= MaxPathInstructions; i++ {
		fmt.Fprintf(&many, "  - {path: \"d%d/**\", instructions: \"x\"}\n", i)
	}
	cases := map[string]string{
		"bad glob":     "path_instructions:\n  - {path: \"migrations/[\", instructions: \"x\"}\n",
		"empty path":   "path_instructions:\n  - {path: \" \", instructions: \"x\"}\n",
		"empty text":   "path_instructions:\n  - {path: \"a/**\", instructions: \"  \"}\n",
		"too long":     "path_instructions:\n  - {path: \"a/**\", instructions: \"" + long + "\"}\n",
		"too many":     many.String(),
		"wrong shape":  "path_instructions: \"migrations/**\"\n",
		"unknown type": "path_instructions:\n  - path: [1, 2]\n    instructions: x\n",
	}
	for name, y := range cases {
		_, err := ParseRepo([]byte(y))
		if err == nil {
			t.Errorf("%s: expected error", name)
			continue
		}
		if !strings.Contains(err.Error(), RepoConfigPath) {
			t.Errorf("%s: error should name %s: %v", name, RepoConfigPath, err)
		}
	}
	// Exactly at the limits is fine.
	ok := strings.Repeat("я", MaxPathInstructionRunes)
	if _, err := ParseRepo([]byte("path_instructions:\n  - {path: \"a/**\", instructions: \"" + ok + "\"}\n")); err != nil {
		t.Fatalf("limit: %v", err)
	}
	if _, err := ParseRepo([]byte("path_instructions:\n  - {path: \"migrations/[\", instructions: \"x\"}\n")); err == nil || !strings.Contains(err.Error(), "invalid glob") {
		t.Fatalf("bad glob message: %v", err)
	}
}

func TestPathInstructionsMerge(t *testing.T) {
	svc := Defaults()
	svc.Review.PathInstructions = []PathInstruction{
		{Path: "**/*.sql", Instructions: "Сервис: SQL"},
		{Path: "bad[", Instructions: "dropped"},
	}
	if got := Merge(&svc, nil).PathInstructions; len(got) != 1 || got[0].Path != "**/*.sql" {
		t.Fatalf("service only: %+v", got)
	}
	repo, err := ParseRepo([]byte("path_instructions:\n  - {path: \"migrations/**\", instructions: \"Репо: миграции\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := Merge(&svc, repo).PathInstructions
	if len(got) != 2 || got[0].Instructions != "Сервис: SQL" || got[1].Path != "migrations/**" {
		t.Fatalf("service first, then repo: %+v", got)
	}
	// Merge must not alias the service slice.
	got[0].Path = "changed"
	if svc.Review.PathInstructions[0].Path != "**/*.sql" {
		t.Fatal("merge aliases the service config")
	}
	if len(Merge(&Service{Review: Review{}}, &Repo{}).PathInstructions) != 0 {
		t.Fatal("empty")
	}
}

func TestServicePathInstructionsValidate(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	os.WriteFile(good, []byte("gitlab:\n  url: https://g.example\nreview:\n  path_instructions:\n    - path: \"api/**\"\n      instructions: \"Проверяй контракты\"\n"), 0o600)
	cfg, err := LoadService(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Review.PathInstructions) != 1 || cfg.Review.PathInstructions[0].Path != "api/**" {
		t.Fatalf("service path_instructions %+v", cfg.Review.PathInstructions)
	}
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte("gitlab:\n  url: https://g.example\nreview:\n  path_instructions:\n    - path: \"api/[\"\n      instructions: \"x\"\n"), 0o600)
	if _, err := LoadService(bad); err == nil || !strings.Contains(err.Error(), "review.path_instructions") {
		t.Fatalf("bad service glob: %v", err)
	}
}

func TestLearnFromActionsDefaults(t *testing.T) {
	d := Defaults()
	if !d.Review.Learn.FromActions || d.Review.Learn.DismissThreshold != 3 {
		t.Fatalf("defaults %+v", d.Review.Learn)
	}
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("gitlab:\n  url: https://g.example\nreview:\n  learn:\n    from_actions: false\n    dismiss_threshold: 5\n"), 0o600)
	cfg, err := LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.Learn.FromActions || cfg.Review.Learn.DismissThreshold != 5 || !cfg.Review.Learn.Enabled {
		t.Fatalf("yaml %+v", cfg.Review.Learn)
	}
}
