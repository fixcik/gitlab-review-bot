package telemetry

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The polling alerts scale their windows with the configured interval and
// tell "no rounds at all" from "rounds without success": both need gauges.
func TestObservePollGauges(t *testing.T) {
	m := New("test")
	has(t, scrape(t, m),
		"reviewbot_poll_interval_seconds 0",
		"reviewbot_poll_last_round_timestamp_seconds 0",
		"reviewbot_poll_last_success_timestamp_seconds 0")
	m.SetPollInterval(10 * time.Minute)
	m.ObservePoll(time.Second, 2, errors.New("403"))
	out := scrape(t, m)
	has(t, out, "reviewbot_poll_interval_seconds 600", `reviewbot_poll_ticks_total{result="error"} 1`,
		"reviewbot_poll_last_success_timestamp_seconds 0")
	if strings.Contains(out, "reviewbot_poll_last_round_timestamp_seconds 0\n") {
		t.Fatalf("a failed round must set the last round time:\n%s", out)
	}
	m.ObservePoll(time.Second, 0, nil)
	if strings.Contains(scrape(t, m), "reviewbot_poll_last_success_timestamp_seconds 0\n") {
		t.Fatal("a successful round must set the last success time")
	}
	var nilM *Metrics
	nilM.SetPollInterval(time.Minute)
	nilM.ObservePoll(0, 0, nil)
}
