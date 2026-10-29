package telemetry

// ObserveReaction counts one award emoji change (reactions.Options.Observe).
// Event and result come from fixed sets. Nil-safe.
func (m *Metrics) ObserveReaction(event, result string) {
	if m == nil {
		return
	}
	m.ReactionsTotal.With(event, result).Inc()
}
