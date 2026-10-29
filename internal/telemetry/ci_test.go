package telemetry

import "testing"

func TestObserveCI(t *testing.T) {
	m := New("test")
	m.ObserveCIFailure("mr", "mr")
	m.ObserveCIFailure("mr", "mr")
	m.ObserveCIFailure("main", "flaky")
	m.ObserveCIExplain("ok")
	m.ObserveCIExplain("duplicate")
	has(t, scrape(t, m),
		`reviewbot_ci_failures_total{scope="mr",cause="mr"} 2`,
		`reviewbot_ci_failures_total{scope="main",cause="flaky"} 1`,
		`reviewbot_ci_explain_runs_total{result="ok"} 1`,
		`reviewbot_ci_explain_runs_total{result="duplicate"} 1`,
	)
	var nilM *Metrics
	nilM.ObserveCIFailure("mr", "mr")
	nilM.ObserveCIExplain("ok")
}
