package telemetry

// ObserveCIFailure counts one failed job of a failed pipeline (review.ci)
// by scope (mr: an MR head pipeline, main: a main branch pipeline) and
// cause (mr, infra, flaky, unknown; unknown when not explained). Nil-safe.
func (m *Metrics) ObserveCIFailure(scope, cause string) {
	if m == nil {
		return
	}
	m.CIFailures.With(scope, cause).Inc()
}

// ObserveCIExplain counts one explanation of a failed pipeline by result
// (ok, limit, rate_limited, invalid_output, error, duplicate). Nil-safe.
func (m *Metrics) ObserveCIExplain(result string) {
	if m == nil {
		return
	}
	m.CIExplainRuns.With(result).Inc()
}
