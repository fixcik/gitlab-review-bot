package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/egress"
)

func runEgress(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("egress", flag.ExitOnError)
	listen := fs.String("listen", ":3128", "listen address")
	cfgPath := fs.String("config", "", "optional config.yaml for sandbox.egress_allow")
	mcpListen := fs.String("mcp-listen", ":8091", "listen address of the MCP port forward (with -mcp-forward)")
	mcpTarget := fs.String("mcp-forward", "", "host:port of the bot's MCP listener (review.mcp.listen) to forward -mcp-listen to; empty = no forward")
	fs.Parse(args)

	allow := config.Defaults().Sandbox.EgressAllow
	if *cfgPath != "" {
		// Only the allowlist is needed; don't require GitLab secrets here.
		cfg, err := config.LoadSandbox(*cfgPath)
		if err != nil {
			return err
		}
		allow = cfg.EgressAllow
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           egress.NewProxy(allow, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
	}()
	if t := strings.TrimSpace(*mcpTarget); t != "" {
		if err := startMCPForward(ctx, *mcpListen, t, logger); err != nil {
			return err
		}
	}
	logger.Info("egress proxy listening", "addr", *listen, "allow", allow)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startMCPForward forwards listen to target in the background. The sandbox
// reaches the bot's MCP endpoint only through this one forwarded port (see
// internal/egress/forward.go).
func startMCPForward(ctx context.Context, listen, target string, logger *slog.Logger) error {
	if _, _, err := net.SplitHostPort(target); err != nil {
		return fmt.Errorf("-mcp-forward %q: %w", target, err)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("-mcp-listen %s: %w", listen, err)
	}
	fw := &egress.Forwarder{Target: target, Log: logger}
	go func() {
		if err := fw.Serve(ctx, ln); err != nil {
			logger.Error("mcp forward stopped", "err", err)
		}
	}()
	logger.Info("mcp port forward listening", "addr", ln.Addr().String(), "target", target)
	return nil
}
