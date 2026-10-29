package telemetry

import "testing"

func TestObserveLabel(t *testing.T) {
	m := New("test")
	m.ObserveLabel("add", "ok")
	m.ObserveLabel("add", "ok")
	m.ObserveLabel("remove", "forbidden")
	has(t, scrape(t, m),
		`reviewbot_labels_total{action="add",result="ok"} 2`,
		`reviewbot_labels_total{action="remove",result="forbidden"} 1`,
	)
	var nilM *Metrics
	nilM.ObserveLabel("add", "ok")
}
