package telemetry

import (
	"testing"
	"time"
)

func TestObserveMCPCall(t *testing.T) {
	m := New("test")
	m.ObserveMCPCall("submit_finding", "ok")
	m.ObserveMCPCall("submit_finding", "ok")
	m.ObserveMCPCall("diff_lines", "limit")
	has(t, scrape(t, m),
		`reviewbot_mcp_tool_calls_total{tool="submit_finding",status="ok"} 2`,
		`reviewbot_mcp_tool_calls_total{tool="diff_lines",status="limit"} 1`,
	)
	var nilM *Metrics
	nilM.ObserveMCPCall("x", "ok")
}

type degraded bool

func (d degraded) Degraded() (bool, time.Time) { return bool(d), time.Time{} }

func TestObserveMCPFallback(t *testing.T) {
	m := New("test")
	m.ObserveMCPFallback("not_connected")
	m.ObserveMCPServer(degraded(true))
	has(t, scrape(t, m),
		`reviewbot_mcp_fallbacks_total{reason="not_connected"} 1`,
		`reviewbot_mcp_degraded 1`,
	)
	var nilM *Metrics
	nilM.ObserveMCPFallback("x")
	nilM.ObserveMCPServer(degraded(true))
}
