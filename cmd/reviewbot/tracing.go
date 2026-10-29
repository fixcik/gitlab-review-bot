package main

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/claude"
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// tracingShutdownTimeout bounds the final flush of queued spans on SIGTERM.
const tracingShutdownTimeout = 5 * time.Second

// startTracing creates the tracer and its OTLP exporter (tracing.*). It
// returns a nil tracer (every hook a no-op) and a no-op shutdown when
// tracing is off or the exporter cannot start: tracing never stops the
// bot. The shutdown flushes queued spans.
func startTracing(cfg *config.Service, tm *telemetry.Metrics, logger *slog.Logger) (*tracing.Tracer, func()) {
	tc := cfg.Tracing
	if !tc.On() {
		return nil, func() {}
	}
	exp, err := tracing.NewExporter(tracing.ExporterOptions{
		Endpoint: tc.Endpoint, Headers: tc.Headers, Timeout: tc.TimeoutD(), CAFile: tc.CAFile, Logger: logger,
		Observer: tm.ObserveTracingSpans, OnError: tm.ObserveTracingError,
	})
	if err != nil {
		logger.Error("tracing disabled: cannot start the exporter", "err", err)
		return nil, func() {}
	}
	ratio := tc.Ratio()
	tr := tracing.New(tracing.Options{
		Resource: tracing.Resource{ServiceName: tc.ServiceName, ServiceVersion: version,
			ServiceInstanceID: tracing.DefaultInstanceID(), Environment: tc.Environment},
		SampleRatio: &ratio,
		Exporter:    exp,
		// Same scrubbing as /runs: configured secrets, token formats,
		// credentials in URLs (the tracer also drops URL queries).
		Scrub: diag.ScrubFunc(diagSecrets(cfg)),
	})
	headerNames := make([]string, 0, len(tc.Headers))
	for k := range tc.Headers {
		headerNames = append(headerNames, k) // names only, never values
	}
	sort.Strings(headerNames)
	logger.Info("tracing enabled", "endpoint", tracing.RedactURL(exp.URL()), "service", tc.ServiceName,
		"environment", tc.Environment, "sample_ratio", ratio, "headers", headerNames, "claude_cli", tc.ClaudeCLI)
	return tr, func() {
		ctx, cancel := context.WithTimeout(context.Background(), tracingShutdownTimeout)
		defer cancel()
		if err := exp.Shutdown(ctx); err != nil {
			logger.Warn("tracing: not every span was flushed on shutdown", "err", err)
		}
	}
}

// tracedLogger adds trace_id/span_id to the log lines of traced jobs
// (unchanged when tracing is off).
func tracedLogger(tr *tracing.Tracer, logger *slog.Logger, active *tracing.Active) *slog.Logger {
	if tr == nil {
		return logger
	}
	return slog.New(tracing.LogHandler(logger.Handler(), active, "mr"))
}

// traceWebhook makes every webhook request a server span (unchanged when
// tracing is off). The GitLab event name is the only request detail.
func traceWebhook(tr *tracing.Tracer, h http.Handler) http.Handler {
	return tr.Handler("/webhook", h, func(r *http.Request) []tracing.Attr {
		if ev := r.Header.Get("X-Gitlab-Event"); ev != "" {
			return []tracing.Attr{tracing.String("gitlab.event", ev)}
		}
		return nil
	})
}

// cliTelemetry is the experimental Claude Code trace export
// (tracing.claude_cli; nil when off).
func cliTelemetry(cfg *config.Service, logger *slog.Logger) *claude.CLITelemetry {
	tc := cfg.Tracing
	if !tc.On() || !tc.ClaudeCLI {
		return nil
	}
	o := claude.CLITelemetryOptions{Endpoint: tc.CLIEndpoint(), Environment: tc.Environment, NoProxyHost: tc.CLIDirectHost()}
	if tc.ClaudeCLIHeaders {
		o.Headers = tc.Headers
		o.PassCredentials = true // user:password@ of the endpoint, as a header
	}
	t := claude.NewCLITelemetry(o)
	if t != nil {
		logger.Warn("tracing.claude_cli is EXPERIMENTAL: Claude Code exports its own spans from the sandbox to the collector",
			"endpoint", tracing.RedactURL(tc.CLIEndpoint()), "via_egress_proxy", tc.CLIEgressHost() != "",
			"direct_host", tc.CLIDirectHost(), "headers", tc.ClaudeCLIHeaders)
	}
	return t
}
