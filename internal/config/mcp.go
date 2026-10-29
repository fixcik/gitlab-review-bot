package config

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// MCP configures the bot's MCP server (review.mcp, see internal/mcpserver):
// during a review the model calls bot-side tools (diff lines, linter
// results, the API changelog, the system map, migration hints, the Jira
// task, related repositories) and submits every finding through a tool that
// validates it at once with the rules the bot applies when publishing.
// Publishing itself stays at the end of the run.
type MCP struct {
	// Enabled turns the MCP server on (default true). A run whose CLI does
	// not connect falls back to the request without MCP (review/
	// mcp_fallback.go); a server that cannot start leaves MCP off with a
	// warning (cmd/reviewbot startMCP).
	Enabled bool `yaml:"enabled"`
	// Listen is the address of the MCP listener: its own port, never the
	// public HTTP listener (default ":8091").
	Listen string `yaml:"listen"`
	// URL is the MCP endpoint as the Claude CLI sees it. Empty = derived:
	// docker mode http://<egress proxy host>:<listen port>/mcp (the egress
	// container forwards that port to the bot), local mode
	// http://127.0.0.1:<listen port>/mcp.
	URL string `yaml:"url"`
	// MaxCalls caps the tool calls of one review run (default 200).
	MaxCalls int `yaml:"max_calls"`
	// MaxResponseKB caps the text of one tool answer (default 64).
	MaxResponseKB int `yaml:"max_response_kb"`
}

func defaultMCP() MCP {
	return MCP{Enabled: true, Listen: ":8091", MaxCalls: 200, MaxResponseKB: 64}
}

// listenPort returns the port of a host:port listen address ("" when bad).
func listenPort(addr string) string {
	_, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return ""
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return ""
	}
	return port
}

func (m MCP) validate(mainListen string) []string {
	var errs []string
	port := listenPort(m.Listen)
	if port == "" {
		errs = append(errs, `review.mcp.listen must be host:port, e.g. ":8091"`)
	} else if port == listenPort(mainListen) {
		errs = append(errs, "review.mcp.listen must use its own port, not the port of listen (the MCP endpoint is never served on the public HTTP listener)")
	}
	if u := strings.TrimSpace(m.URL); u != "" && !validBaseURL(u) {
		errs = append(errs, "review.mcp.url must be an http(s) URL without credentials, query or fragment, e.g. http://host.docker.internal:8091/mcp")
	}
	if m.MaxCalls < 1 || m.MaxCalls > 2000 {
		errs = append(errs, "review.mcp.max_calls must be between 1 and 2000")
	}
	if m.MaxResponseKB < 4 || m.MaxResponseKB > 1024 {
		errs = append(errs, "review.mcp.max_response_kb must be between 4 and 1024")
	}
	return errs
}

// MCPClientURL is the MCP endpoint the Claude CLI connects to: review.mcp.url
// when set, else derived from the sandbox mode (see MCP.URL).
func (c *Service) MCPClientURL() string {
	m := c.Review.MCP
	if u := strings.TrimSpace(m.URL); u != "" {
		return u
	}
	port := listenPort(m.Listen)
	if port == "" {
		return ""
	}
	host := "127.0.0.1"
	if c.Sandbox.Mode != "local" {
		pu, err := url.Parse(c.Sandbox.EgressProxy)
		if err != nil || pu.Hostname() == "" {
			return ""
		}
		host = pu.Hostname()
	}
	return "http://" + net.JoinHostPort(host, port) + "/mcp"
}

// RepoMCP is the mcp section of .reviewbot.yaml: a repository may only turn
// the MCP tools off.
type RepoMCP struct {
	Enabled *bool `yaml:"enabled"`
}

func (r *Repo) mcpEnabled() bool {
	return r == nil || r.MCP == nil || defaultTrue(r.MCP.Enabled)
}
