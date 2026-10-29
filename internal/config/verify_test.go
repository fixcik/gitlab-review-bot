package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyDefaults(t *testing.T) {
	d := Defaults()
	v := d.Review.Verify
	if !v.Enabled || strings.Join(v.Severities, ",") != "critical,major" || v.MaxFindings != 10 || v.Model != "" || v.Timeout != 0 {
		t.Errorf("defaults %+v", v)
	}
	if errs := v.validate(); len(errs) > 0 {
		t.Errorf("defaults invalid: %v", errs)
	}
	if s := v.SeveritySet(); !s["critical"] || !s["major"] || s["minor"] {
		t.Errorf("set %v", s)
	}
}

func TestVerifyValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Verify)
		want string
	}{
		{"severity", func(v *Verify) { v.Severities = []string{"critical", "blocker"} }, `"blocker" is not`},
		{"empty", func(v *Verify) { v.Severities = nil }, "must not be empty"},
		{"max", func(v *Verify) { v.MaxFindings = 0 }, "review.verify.max_findings"},
		{"max high", func(v *Verify) { v.MaxFindings = 51 }, "review.verify.max_findings"},
		{"timeout short", func(v *Verify) { v.Timeout = Duration(10 * time.Second) }, "review.verify.timeout"},
		{"timeout long", func(v *Verify) { v.Timeout = Duration(3 * time.Hour) }, "review.verify.timeout"},
	} {
		v := defaultVerify()
		tc.edit(&v)
		if errs := strings.Join(v.validate(), "; "); !strings.Contains(errs, tc.want) {
			t.Errorf("%s: errors %q, want %q", tc.name, errs, tc.want)
		}
	}
	v := defaultVerify()
	v.Enabled, v.Severities = false, nil
	if errs := v.validate(); len(errs) > 0 {
		t.Errorf("off without severities is fine: %v", errs)
	}
}

// A service config replaces the severities list (not appended to the
// defaults); the repository can only turn the verification off.
func TestVerifyLoadAndMerge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("review:\n  verify:\n    severities: [critical]\n    model: opus\n    timeout: 5m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := readService(p)
	if err != nil {
		t.Fatal(err)
	}
	v := c.Review.Verify
	if !v.Enabled || strings.Join(v.Severities, ",") != "critical" || v.Model != "opus" || v.Timeout.D() != 5*time.Minute || v.MaxFindings != 10 {
		t.Fatalf("loaded %+v", v)
	}
	if !Merge(c, nil).Verify {
		t.Error("on by default")
	}
	off, err := ParseRepo([]byte("verify:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Merge(c, off).Verify {
		t.Error("repo off")
	}
	on, _ := ParseRepo([]byte("verify:\n  enabled: true\n"))
	c.Review.Verify.Enabled = false
	if Merge(c, on).Verify {
		t.Error("a repository cannot turn it on")
	}
}
