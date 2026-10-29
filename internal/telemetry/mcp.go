package telemetry

import "time"

// ObserveMCPCall counts one tool call to the bot's MCP server
// (mcpserver.Options.OnCall). Nil-safe.
func (m *Metrics) ObserveMCPCall(tool, status string) {
	if m == nil {
		return
	}
	m.MCPToolCalls.With(tool, status).Inc()
}

// ObserveMCPFallback counts one review run that fell back to the request
// without MCP because the CLI did not use the bot's MCP server
// (mcpserver.Options.OnFallback). Nil-safe.
func (m *Metrics) ObserveMCPFallback(reason string) {
	if m == nil {
		return
	}
	m.MCPFallbacks.With(reason).Inc()
}

// MCPDegraded is implemented by *mcpserver.Server.
type MCPDegraded interface {
	Degraded() (bool, time.Time)
}

// ObserveMCPServer samples whether new review runs skip MCP after a
// fallback (the circuit breaker) at every scrape. Nil-safe.
func (m *Metrics) ObserveMCPServer(s MCPDegraded) {
	if m == nil || s == nil {
		return
	}
	m.Registry.OnCollect(func() {
		v := 0.0
		if on, _ := s.Degraded(); on {
			v = 1
		}
		m.MCPDegraded.Set(v)
	})
}
