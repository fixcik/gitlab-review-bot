package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

// The system map handed to the reviewer exposes the whole index, which
// map_lookup searches (restricted to the repositories of the run).
var _ interface{ Index() *sysmap.Index } = (*sysmap.Map)(nil)
var _ crossrepo.SystemMap = (*sysmap.Map)(nil)

func TestStartMCP(t *testing.T) {
	cfg := config.Defaults()
	cfg.Review.MCP.Enabled = false
	srv, stop := startMCP(&cfg, nil, slog.New(slog.DiscardHandler))
	if srv != nil {
		t.Fatalf("off: %v", srv)
	}
	stop()
	cfg = config.Defaults() // on by default
	// Port 0 derives no client URL: name it.
	cfg.Review.MCP.Listen, cfg.Review.MCP.URL = "127.0.0.1:0", "http://egress:8091/mcp"
	srv, stop = startMCP(&cfg, nil, slog.New(slog.DiscardHandler))
	if srv == nil {
		t.Fatal("on: no server")
	}
	defer stop()
	// What cannot be served leaves MCP off instead of failing the start.
	cfg.Review.MCP.Listen = "256.0.0.1:1"
	if srv, stop := startMCP(&cfg, nil, slog.New(slog.DiscardHandler)); srv != nil {
		stop()
		t.Error("bad listen address: server started")
	}
	cfg.Review.MCP.Listen, cfg.Review.MCP.URL = "127.0.0.1:0", ""
	cfg.Sandbox.EgressProxy = ""
	if srv, stop := startMCP(&cfg, nil, slog.New(slog.DiscardHandler)); srv != nil {
		stop()
		t.Error("no client URL: server started")
	}
	// Recording a call without metrics or a diagnostics run is a no-op.
	onMCPCall(nil)(context.Background(), mcpserver.CallInfo{Tool: "diff_lines", Status: mcpserver.StatusOK, Duration: time.Millisecond})
}
