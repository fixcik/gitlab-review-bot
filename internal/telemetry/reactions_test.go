package telemetry

import "testing"

func TestObserveReaction(t *testing.T) {
	m := New("test")
	m.ObserveReaction("picked_up", "added")
	m.ObserveReaction("picked_up", "added")
	m.ObserveReaction("thanks", "forbidden")
	has(t, scrape(t, m),
		`reviewbot_reactions_total{event="picked_up",result="added"} 2`,
		`reviewbot_reactions_total{event="thanks",result="forbidden"} 1`,
	)
	var nilM *Metrics
	nilM.ObserveReaction("done", "added")
}
