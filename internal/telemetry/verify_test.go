package telemetry

import "testing"

func TestObserveVerify(t *testing.T) {
	m := New("test")
	m.ObserveVerifyRun("ok")
	m.ObserveVerifyRun("ok")
	m.ObserveVerifyRun("invalid_output")
	m.ObserveVerifyVerdict("reject", "critical")
	m.ObserveVerifyVerdict("downgrade", "major")
	m.ObserveVerifyVerdict("downgrade", "major")
	has(t, scrape(t, m),
		`reviewbot_verify_runs_total{result="ok"} 2`,
		`reviewbot_verify_runs_total{result="invalid_output"} 1`,
		`reviewbot_verify_verdicts_total{verdict="reject",severity="critical"} 1`,
		`reviewbot_verify_verdicts_total{verdict="downgrade",severity="major"} 2`,
	)
	var nilM *Metrics
	nilM.ObserveVerifyRun("ok")
	nilM.ObserveVerifyVerdict("confirm", "major")
}
