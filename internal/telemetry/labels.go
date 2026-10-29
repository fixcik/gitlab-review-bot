package telemetry

// ObserveLabel counts one label change of a review (review.labels): action
// add or remove; result ok, forbidden, missing, ignored or error. Nil-safe.
func (m *Metrics) ObserveLabel(action, result string) {
	if m == nil {
		return
	}
	m.LabelsTotal.With(action, result).Inc()
}
