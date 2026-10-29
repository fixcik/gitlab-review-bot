package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
)

// startMCP starts the MCP server for review tools on its own listener
// (review.mcp.listen; never the public mux). It returns nil and a no-op
// stop when review.mcp is off or cannot be served: no client URL for the
// sandbox, or the listener cannot be opened. Those are warnings, not
// startup errors: MCP is on by default and the review works without it
// (the request is then exactly the one of MCP off). A deployment whose
// sandbox cannot reach a running server is caught per run instead
// (review/mcp/fallback.go).
func startMCP(cfg *config.Service, tm *telemetry.Metrics, logger *slog.Logger) (*mcpserver.Server, func()) {
	m := cfg.Review.MCP
	if !m.Enabled {
		return nil, func() {}
	}
	if cfg.MCPClientURL() == "" {
		logger.Warn("mcp: no address of the MCP endpoint for the sandbox (review.mcp.url, or sandbox.egress_proxy in docker mode): MCP stays off")
		return nil, func() {}
	}
	ln, err := net.Listen("tcp", m.Listen)
	if err != nil {
		logger.Warn("mcp: cannot listen, MCP stays off (reviews run without the bot's tools)", "listen", m.Listen, "err", err)
		return nil, func() {}
	}
	srv := mcpserver.New(mcpserver.Options{
		MaxCalls:         m.MaxCalls,
		MaxResponseBytes: m.MaxResponseKB << 10,
		Version:          version,
		Logger:           logger,
		OnCall:           onMCPCall(tm),
		// A run whose CLI did not connect (review/mcp/fallback.go).
		OnFallback: tm.ObserveMCPFallback,
	})
	tm.ObserveMCPServer(srv) // reviewbot_mcp_degraded at scrape time
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// A tool call is bounded by mcpserver.DefaultCallTimeout.
		WriteTimeout: mcpserver.DefaultCallTimeout + 30*time.Second,
		IdleTimeout:  2 * time.Minute,
	}
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("mcp server stopped", "err", err)
		}
	}()
	logger.Info("mcp server listening", "addr", ln.Addr().String(), "client_url", cfg.MCPClientURL(),
		"max_calls", m.MaxCalls, "max_response_kb", m.MaxResponseKB)
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	}
	return srv, stop
}

// onMCPCall records each tool call: a metric, and a step in the job's
// timeline on /runs (tool, status, duration, size, short argument preview;
// never the token).
func onMCPCall(tm *telemetry.Metrics) func(ctx context.Context, c mcpserver.CallInfo) {
	return func(ctx context.Context, c mcpserver.CallInfo) {
		tm.ObserveMCPCall(c.Tool, c.Status)
		diag.Note(ctx, "mcp: "+c.Tool, diag.Fields{
			"status": c.Status, "duration_ms": c.Duration.Milliseconds(), "bytes": c.Bytes, "args": c.ArgsPreview,
		})
	}
}
