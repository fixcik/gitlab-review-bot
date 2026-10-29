package config

import (
	"strings"
	"testing"
	"time"
)

// The limiter keeps usage for 24h: a longer cooldown would stop working.
func TestRerunCooldownCappedAt24h(t *testing.T) {
	svc := Defaults()
	repo, err := ParseRepo([]byte("limits:\n  mr_rerun_cooldown: 72h\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(&svc, repo).Limits.MRRerunCooldown.D(); got != MaxRerunCooldown {
		t.Errorf("repo cooldown 72h -> %v, want %v", got, MaxRerunCooldown)
	}
	repo, _ = ParseRepo([]byte("limits:\n  mr_rerun_cooldown: 2h\n"))
	if got := Merge(&svc, repo).Limits.MRRerunCooldown.D(); got != 2*time.Hour {
		t.Errorf("repo cooldown 2h -> %v", got)
	}
	for _, d := range []time.Duration{25 * time.Hour, -time.Minute} {
		s := Defaults()
		s.Limits.MRRerunCooldown = Duration(d)
		err := s.Validate()
		if err == nil || !strings.Contains(err.Error(), "limits.mr_rerun_cooldown") {
			t.Errorf("cooldown %v: err = %v", d, err)
		}
	}
	s := Defaults()
	s.Limits.MRRerunCooldown = Duration(MaxRerunCooldown)
	if err := s.Validate(); err != nil && strings.Contains(err.Error(), "mr_rerun_cooldown") {
		t.Errorf("24h must be valid: %v", err)
	}
}
