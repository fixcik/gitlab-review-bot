package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

func TestStartTracing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Defaults()
	tr, stop := startTracing(&cfg, nil, logger)
	stop()
	if tr != nil || cliTelemetry(&cfg, logger) != nil {
		t.Fatal("tracing off but a tracer was built")
	}
	if l := tracedLogger(nil, logger, nil); l != logger {
		t.Fatal("logger wrapped with tracing off")
	}

	var got atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		auth.Store(r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	cfg.Tracing.Enabled = true
	cfg.Tracing.Endpoint = srv.URL
	cfg.Tracing.Headers = map[string]string{"Authorization": "Bearer t0ken-value-123"}
	tm := telemetry.New("test")
	tr, stop = startTracing(&cfg, tm, logger)
	if tr == nil {
		t.Fatal("no tracer")
	}
	_, sp := tr.Start(context.Background(), "review.job")
	sp.End()
	stop() // flushes
	if got.Load() != 1 || auth.Load() != "Bearer t0ken-value-123" {
		t.Fatalf("export requests %d auth %v", got.Load(), auth.Load())
	}
	if v := tm.TracingSpans.With(tracing.ResultExported).Value(); v != 1 {
		t.Fatalf("exported metric = %v", v)
	}
	// The exporter's credentials are scrubbed like every other secret.
	found := false
	for _, s := range diagSecrets(&cfg) {
		if s == "t0ken-value-123" {
			found = true
		}
	}
	if !found {
		t.Fatal("tracing header credential not in the scrub list")
	}

	// An exporter that cannot start (bad CA file) leaves the bot running
	// without tracing.
	cfg.Tracing.CAFile = "/nonexistent/ca.pem"
	if tr, stop := startTracing(&cfg, nil, logger); tr != nil {
		stop()
		t.Fatal("tracer despite a broken exporter")
	}

	cfg.Tracing.CAFile = ""
	cfg.Tracing.ClaudeCLI = true
	if cliTelemetry(&cfg, logger) == nil {
		t.Fatal("claude_cli on but no CLI telemetry")
	}
	// The endpoint's user:password@ never goes into the CLI's endpoint; with
	// claude_cli_headers it travels as an Authorization header.
	cfg.Tracing.ClaudeCLIEndpoint = "http://u:pw-secret@tempo:4318"
	if _, ok := cliTelemetry(&cfg, logger).SecretEnv(); ok {
		t.Fatal("credentials passed without claude_cli_headers")
	}
	cfg.Tracing.ClaudeCLIHeaders = true
	cfg.Tracing.Headers = nil
	tel := cliTelemetry(&cfg, logger)
	if kv, _ := tel.SecretEnv(); !strings.Contains(kv, "Authorization=Basic%20") ||
		strings.Contains(strings.Join(tel.Env(context.Background()), " "), "pw-secret") {
		t.Fatalf("claude_cli_headers: %q", kv)
	}
}
