package telemetry

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

func TestTracingMetrics(t *testing.T) {
	var nilM *Metrics
	nilM.ObserveTracingSpans(tracing.ResultExported, 1) // nil-safe
	nilM.ObserveTracingError(tracing.ErrReasonNetwork)

	m := New("test")
	m.ObserveTracingSpans(tracing.ResultExported, 5)
	m.ObserveTracingSpans(tracing.ResultDropped, 2)
	m.ObserveTracingSpans(tracing.ResultFailed, 0)
	m.ObserveTracingError(tracing.ErrReasonThrottle)
	var buf bytes.Buffer
	if err := m.Registry.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{`reviewbot_tracing_spans_total{result="exported"} 5`, `reviewbot_tracing_spans_total{result="dropped"} 2`,
		`reviewbot_tracing_export_errors_total{reason="throttled"} 1`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	// The label values used by the exporter are documented in HELP (the
	// dashboard test checks queries against HELP words).
	for _, v := range []string{tracing.ResultExported, tracing.ResultDropped, tracing.ResultFailed} {
		if !strings.Contains(out, v+" (") {
			t.Errorf("HELP does not document result %q", v)
		}
	}
	for _, v := range []string{tracing.ErrReasonNetwork, tracing.ErrReasonThrottle, tracing.ErrReasonServer, tracing.ErrReasonClient, tracing.ErrReasonPartial} {
		if !strings.Contains(out, v+" (") {
			t.Errorf("HELP does not document reason %q", v)
		}
	}
}
