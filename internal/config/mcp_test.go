package config

import (
	"strings"
	"testing"
)

func TestMCPDefaults(t *testing.T) {
	d := Defaults()
	m := d.Review.MCP
	if !m.Enabled || m.Listen != ":8091" || m.MaxCalls != 200 || m.MaxResponseKB != 64 || m.URL != "" {
		t.Errorf("defaults %+v", m)
	}
	if errs := m.validate(d.Listen); len(errs) > 0 {
		t.Errorf("defaults invalid: %v", errs)
	}
}

func TestMCPValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*MCP)
		want string
	}{
		{"bad listen", func(m *MCP) { m.Listen = "8091" }, "review.mcp.listen must be host:port"},
		{"bad port", func(m *MCP) { m.Listen = ":99999" }, "review.mcp.listen must be host:port"},
		{"same port as http", func(m *MCP) { m.Listen = "0.0.0.0:8080" }, "own port"},
		{"url scheme", func(m *MCP) { m.URL = "ftp://x/mcp" }, "review.mcp.url"},
		{"url credentials", func(m *MCP) { m.URL = "http://u:p@x:8091/mcp" }, "review.mcp.url"},
		{"url query", func(m *MCP) { m.URL = "http://x:8091/mcp?token=1" }, "review.mcp.url"},
		{"calls", func(m *MCP) { m.MaxCalls = 0 }, "review.mcp.max_calls"},
		{"calls high", func(m *MCP) { m.MaxCalls = 5000 }, "review.mcp.max_calls"},
		{"kb", func(m *MCP) { m.MaxResponseKB = 2 }, "review.mcp.max_response_kb"},
	} {
		m := defaultMCP()
		tc.edit(&m)
		errs := strings.Join(m.validate(":8080"), "; ")
		if !strings.Contains(errs, tc.want) {
			t.Errorf("%s: errors %q, want %q", tc.name, errs, tc.want)
		}
	}
	m := defaultMCP()
	m.URL = "http://host.docker.internal:8091/mcp"
	m.Listen = "127.0.0.1:9000"
	if errs := m.validate(":8080"); len(errs) > 0 {
		t.Errorf("valid config rejected: %v", errs)
	}
}

func TestMCPClientURL(t *testing.T) {
	c := Defaults()
	if got := c.MCPClientURL(); got != "http://egress:8091/mcp" {
		t.Errorf("docker default %q", got)
	}
	c.Sandbox.EgressProxy = "http://proxy.internal:3128"
	c.Review.MCP.Listen = "0.0.0.0:9100"
	if got := c.MCPClientURL(); got != "http://proxy.internal:9100/mcp" {
		t.Errorf("docker custom %q", got)
	}
	c.Sandbox.Mode = "local"
	if got := c.MCPClientURL(); got != "http://127.0.0.1:9100/mcp" {
		t.Errorf("local %q", got)
	}
	c.Review.MCP.URL = " http://host.docker.internal:9100/mcp "
	if got := c.MCPClientURL(); got != "http://host.docker.internal:9100/mcp" {
		t.Errorf("override %q", got)
	}
}

func TestMCPRepoCanOnlyDisable(t *testing.T) {
	svc := Defaults()
	svc.Review.MCP.Enabled = false
	off, on := false, true
	if e := Merge(&svc, &Repo{MCP: &RepoMCP{Enabled: &on}}); e.MCP {
		t.Error("a repository turned MCP on against the service")
	}
	svc.Review.MCP.Enabled = true
	if e := Merge(&svc, nil); !e.MCP {
		t.Error("service on, no repo config: off")
	}
	if e := Merge(&svc, &Repo{MCP: &RepoMCP{Enabled: &off}}); e.MCP {
		t.Error("repo mcp.enabled: false ignored")
	}
	if def := Defaults(); !Merge(&def, nil).MCP {
		t.Error("MCP is on by default")
	}
	r, err := ParseRepo([]byte("mcp:\n  enabled: false\n"))
	if err != nil || r.MCP == nil || r.MCP.Enabled == nil || *r.MCP.Enabled {
		t.Errorf("parse: %+v %v", r, err)
	}
	cfg := Defaults()
	cfg.GitLab.URL, cfg.GitLab.Token, cfg.GitLab.WebhookSecret = "https://gl", "t", "s"
	cfg.Review.MCP.Listen = ":8080"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "review.mcp.listen") {
		t.Errorf("Validate: %v", err)
	}
}

// MCP is on by default: the default config validates and gives the CLI a
// URL in both sandbox modes (docker: through the egress forward; local:
// the loopback port of the listener).
func TestMCPOnByDefaultValid(t *testing.T) {
	for mode, want := range map[string]string{"docker": "http://egress:8091/mcp", "local": "http://127.0.0.1:8091/mcp"} {
		cfg := Defaults()
		cfg.GitLab.URL, cfg.GitLab.Token, cfg.GitLab.WebhookSecret = "https://gl", "t", "s"
		cfg.Sandbox.Mode = mode
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: default config invalid: %v", mode, err)
		}
		if !cfg.Review.MCP.Enabled || cfg.MCPClientURL() != want {
			t.Errorf("%s: enabled %v, url %q", mode, cfg.Review.MCP.Enabled, cfg.MCPClientURL())
		}
	}
}
