package telemetry

// ObserveVerifyRun counts one verification run of a review by result (see
// verify.Observer). Nil-safe.
func (m *Metrics) ObserveVerifyRun(result string) {
	if m == nil {
		return
	}
	m.VerifyRuns.With(result).Inc()
}

// ObserveVerifyVerdict counts one verdict of a verification run by verdict
// and the item's original severity. Nil-safe.
func (m *Metrics) ObserveVerifyVerdict(verdict, severity string) {
	if m == nil {
		return
	}
	m.VerifyVerdicts.With(verdict, severity).Inc()
}
