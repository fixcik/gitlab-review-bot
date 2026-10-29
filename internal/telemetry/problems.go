package telemetry

// ObserveProblem counts one recorded verification problem
// (problems.Options.OnRecord); fresh = new or re-appeared after an
// acknowledgement. Kind and source come from fixed sets. Nil-safe.
func (m *Metrics) ObserveProblem(kind, source string, fresh bool) {
	if m == nil {
		return
	}
	m.ProblemsTotal.With(kind, source).Inc()
	if fresh {
		m.ProblemsNew.With(kind).Inc()
	}
}
