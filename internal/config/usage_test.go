package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageDefaults(t *testing.T) {
	t.Setenv("GITLAB_URL", "https://g.example")
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("USAGE_TOKEN", "")
	cfg, err := LoadService("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Usage.On() || cfg.Usage.Token != "" || cfg.Usage.RetentionD() != 365*24*time.Hour {
		t.Fatalf("defaults: %+v", cfg.Usage)
	}
}

func TestUsageFromFileAndEnv(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("USAGE_TOKEN", "")
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("gitlab:\n  url: https://g.example\nusage:\n  enabled: false\n  token: \" file-tok \"\n  retention: 720h\n"), 0o600)
	cfg, err := LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Usage.On() || cfg.Usage.Token != "file-tok" || cfg.Usage.RetentionD() != 720*time.Hour {
		t.Fatalf("file: %+v", cfg.Usage)
	}
	t.Setenv("USAGE_TOKEN", "env-tok")
	cfg, err = LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Usage.Token != "env-tok" {
		t.Fatalf("env override: %q", cfg.Usage.Token)
	}
}

func TestUsageBudgetValidate(t *testing.T) {
	for _, tc := range []struct {
		v  float64
		ok bool
	}{
		{0, true}, {0.01, true}, {50, true}, {MaxUsageBudgetUSD, true},
		{-0.01, false}, {-50, false}, {MaxUsageBudgetUSD + 1, false},
		{math.NaN(), false}, {math.Inf(1), false}, {math.Inf(-1), false},
	} {
		errs := Usage{BudgetUSD: tc.v}.validate()
		if (len(errs) == 0) != tc.ok {
			t.Errorf("budget %v: errors %v, want ok=%v", tc.v, errs, tc.ok)
		}
		if !tc.ok && !strings.Contains(strings.Join(errs, ";"), "usage.budget_usd") {
			t.Errorf("budget %v: error does not name the key: %v", tc.v, errs)
		}
	}
}

// usage.budget_usd is read from the file (0 when unset) and LoadService
// refuses a negative or absurd budget.
func TestUsageBudgetLoad(t *testing.T) {
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("USAGE_TOKEN", "")
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(p, []byte("gitlab:\n  url: https://g.example\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := LoadService(write(""))
	if err != nil || cfg.Usage.BudgetUSD != 0 {
		t.Fatalf("default: %v %v", cfg.Usage.BudgetUSD, err)
	}
	cfg, err = LoadService(write("usage:\n  budget_usd: 120.5\n"))
	if err != nil || cfg.Usage.BudgetUSD != 120.5 {
		t.Fatalf("file: %v %v", cfg.Usage.BudgetUSD, err)
	}
	for _, bad := range []string{"-1", "2000000", ".nan", ".inf"} {
		if _, err := LoadService(write("usage:\n  budget_usd: " + bad + "\n")); err == nil || !strings.Contains(err.Error(), "usage.budget_usd") {
			t.Errorf("budget %s: err %v", bad, err)
		}
	}
}
