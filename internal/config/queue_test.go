package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueueDefaultsAndFile(t *testing.T) {
	d := Defaults()
	if d.Queue.ShutdownGrace.D() != 5*time.Minute || d.Queue.ManualMaxAge.D() != 6*time.Hour {
		t.Fatalf("defaults %+v", d.Queue)
	}
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	p := filepath.Join(t.TempDir(), "c.yaml")
	// 0 is a real value (cancel at once), not "unset".
	os.WriteFile(p, []byte("gitlab:\n  url: https://g.example\nqueue:\n  shutdown_grace: 0s\n  manual_max_age: 30m\n"), 0o600)
	cfg, err := LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Queue.ShutdownGrace.D() != 0 || cfg.Queue.ManualMaxAge.D() != 30*time.Minute {
		t.Fatalf("file values not applied: %+v", cfg.Queue)
	}
}

func TestQueueValidation(t *testing.T) {
	for _, tc := range []struct {
		q    Queue
		want string
	}{
		{Queue{ShutdownGrace: Duration(-time.Second), ManualMaxAge: Duration(time.Hour)}, "queue.shutdown_grace"},
		{Queue{ShutdownGrace: Duration(2 * time.Hour), ManualMaxAge: Duration(time.Hour)}, "queue.shutdown_grace"},
		{Queue{ShutdownGrace: Duration(time.Minute)}, "queue.manual_max_age"},
		{Queue{ManualMaxAge: Duration(-time.Hour)}, "queue.manual_max_age"},
	} {
		errs := tc.q.validate()
		if len(errs) != 1 || !strings.Contains(errs[0], tc.want) {
			t.Errorf("%+v: %v, want one error about %s", tc.q, errs, tc.want)
		}
	}
	if errs := defaultQueue().validate(); len(errs) != 0 {
		t.Fatalf("defaults invalid: %v", errs)
	}
	if errs := (Queue{ManualMaxAge: Duration(time.Minute)}).validate(); len(errs) != 0 {
		t.Fatalf("grace 0 must be valid: %v", errs)
	}
}
