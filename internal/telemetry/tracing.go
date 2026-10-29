package telemetry

// ObserveTracingSpans counts exported/dropped/failed spans
// (tracing.ExporterOptions.Observer). Nil-safe.
func (m *Metrics) ObserveTracingSpans(result string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.TracingSpans.With(result).Add(float64(n))
}

// ObserveTracingError counts one failed export request
// (tracing.ExporterOptions.OnError). Nil-safe.
func (m *Metrics) ObserveTracingError(reason string) {
	if m == nil {
		return
	}
	m.TracingErrors.With(reason).Inc()
}
