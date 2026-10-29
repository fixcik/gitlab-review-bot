package telemetry

import "testing"

func TestObserveProblem(t *testing.T) {
	m := New("test")
	m.ObserveProblem("lint_failed", "bot", true)
	m.ObserveProblem("lint_failed", "bot", false)
	m.ObserveProblem("model_no_access", "model", true)
	has(t, scrape(t, m),
		`reviewbot_problems_total{kind="lint_failed",source="bot"} 2`,
		`reviewbot_problems_total{kind="model_no_access",source="model"} 1`,
		`reviewbot_problems_new_total{kind="lint_failed"} 1`,
		`reviewbot_problems_new_total{kind="model_no_access"} 1`,
	)
	var nilM *Metrics
	nilM.ObserveProblem("x", "bot", true)
}
