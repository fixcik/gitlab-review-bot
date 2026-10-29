package deploy_test

import (
	"os"
	"os/exec"
	"testing"
)

// TestAlertRulesPromtool runs the alert unit tests
// (prometheus/tests/reviewbot-alerts.test.yml: QueueStuck ignores waiting
// jobs and draining, the polling windows scale with gitlab.poll_interval)
// with promtool: $PROMTOOL or promtool in PATH; skipped without it.
func TestAlertRulesPromtool(t *testing.T) {
	bin := os.Getenv("PROMTOOL")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("promtool"); err != nil {
			t.Skip("promtool not installed (set PROMTOOL to run the alert unit tests)")
		}
	}
	for _, args := range [][]string{
		{"check", "rules", "reviewbot-alerts.yml"},
		{"test", "rules", "tests/reviewbot-alerts.test.yml"},
	} {
		cmd := exec.Command(bin, args...)
		cmd.Dir = "prometheus"
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("promtool %v: %v\n%s", args, err, out)
		}
	}
}
