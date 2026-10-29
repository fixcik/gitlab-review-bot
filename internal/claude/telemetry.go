package claude

import (
	"context"
	"encoding/base64"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Claude Code's own OpenTelemetry traces (tracing.claude_cli,
// EXPERIMENTAL). Claude Code documents (code.claude.com/docs, "Monitoring"):
// CLAUDE_CODE_ENABLE_TELEMETRY=1 plus CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1
// turn span tracing on, OTEL_TRACES_EXPORTER=otlp with
// OTEL_EXPORTER_OTLP_*_PROTOCOL / _ENDPOINT / _HEADERS select the exporter,
// and in `-p` sessions the CLI reads TRACEPARENT from its environment, so
// its claude_code.interaction span becomes a child of the bot's Claude-run
// span. Prompt text and tool details are redacted by the CLI by default;
// the bot never sets the OTEL_LOG_* switches that would include them.
// Metrics and log events of the CLI stay off. Not verified against a live
// CLI in this repository's tests: only the environment is.

// CLITelemetryOptions configure Claude Code's trace export.
type CLITelemetryOptions struct {
	// Endpoint is the collector as the CLI sees it (base URL; /v1/traces is
	// appended unless present).
	Endpoint string
	// Headers are given to the CLI only when the operator asked for it
	// (tracing.claude_cli_headers); nil otherwise.
	Headers map[string]string
	// PassCredentials (tracing.claude_cli_headers): user:password@ of the
	// endpoint goes to the CLI as an "Authorization: Basic" header (unless
	// Headers already set Authorization). Without it the CLI never gets
	// them. Either way the userinfo is stripped from the endpoint variable,
	// which is a literal `docker run -e` argument.
	PassCredentials bool
	// Environment is deployment.environment of the CLI's resource.
	Environment string
	// NoProxyHost: the collector is reached directly from the sandbox (an
	// http endpoint): it joins NO_PROXY so the egress proxy is bypassed.
	NoProxyHost string
}

// CLITelemetry is the environment that turns the CLI's traces on.
type CLITelemetry struct {
	env         []string // literal NAME=value pairs (no secrets)
	headers     string   // OTEL_EXPORTER_OTLP_TRACES_HEADERS value; "" = none
	noProxyHost string
}

// HeadersEnv is the variable that carries the exporter headers to the CLI.
// It is a service secret (core.IsServiceSecretEnv), so the bot passes it
// explicitly and by name only.
const HeadersEnv = "OTEL_EXPORTER_OTLP_TRACES_HEADERS"

// NewCLITelemetry builds the CLI environment; nil for an invalid endpoint.
func NewCLITelemetry(o CLITelemetryOptions) *CLITelemetry {
	traces, err := tracing.TracesURL(o.Endpoint)
	if err != nil {
		return nil
	}
	// TracesURL keeps user:password@ (the bot's own exporter sends it as
	// basic auth); the CLI's endpoint is a literal docker argument, so it
	// never carries them.
	u, _ := url.Parse(traces) // TracesURL's output always parses
	userinfo := u.User
	u.User = nil
	t := &CLITelemetry{noProxyHost: o.NoProxyHost, env: []string{
		"CLAUDE_CODE_ENABLE_TELEMETRY=1",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1",
		"OTEL_TRACES_EXPORTER=otlp",
		"OTEL_METRICS_EXPORTER=none",
		"OTEL_LOGS_EXPORTER=none",
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/json",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/json",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + u.String(),
	}}
	res := []string{"service.namespace=gitlab-review-bot"}
	if o.Environment != "" {
		res = append(res, "deployment.environment="+url.QueryEscape(o.Environment))
	}
	t.env = append(t.env, "OTEL_RESOURCE_ATTRIBUTES="+strings.Join(res, ","))
	headers := maps.Clone(o.Headers)
	if o.PassCredentials && userinfo != nil && !hasHeader(headers, "Authorization") {
		pw, _ := userinfo.Password()
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(userinfo.Username()+":"+pw))
	}
	if len(headers) > 0 {
		keys := make([]string, 0, len(headers))
		for k := range headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var kv []string
		for _, k := range keys {
			kv = append(kv, k+"="+headerValueEscape(headers[k]))
		}
		t.headers = strings.Join(kv, ",")
	}
	return t
}

// headerValueEscape percent-encodes a value of OTEL_EXPORTER_OTLP_*_HEADERS.
// The OTel SDKs decode it with decodeURIComponent (JavaScript, Claude Code),
// which leaves "+" as is: a space must be %20, not QueryEscape's "+".
func headerValueEscape(v string) string {
	return strings.ReplaceAll(url.QueryEscape(v), "+", "%20")
}

func hasHeader(h map[string]string, name string) bool {
	for k := range h {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// Env returns the literal environment of one run: the static settings
// plus TRACEPARENT of the run's span in ctx (when sampled). Headers are not
// included (see SecretEnv).
func (t *CLITelemetry) Env(ctx context.Context) []string {
	if t == nil {
		return nil
	}
	env := append([]string(nil), t.env...)
	if tp := tracing.TraceParent(ctx); tp != "" {
		env = append(env, "TRACEPARENT="+tp)
	}
	return env
}

// SecretEnv returns HeadersEnv=<headers> when headers are configured.
func (t *CLITelemetry) SecretEnv() (string, bool) {
	if t == nil || t.headers == "" {
		return "", false
	}
	return HeadersEnv + "=" + t.headers, true
}

// NoProxyHost is the collector host reached without the egress proxy.
func (t *CLITelemetry) NoProxyHost() string {
	if t == nil {
		return ""
	}
	return t.noProxyHost
}

// withTelemetryDocker adds the CLI telemetry to a review container spec
// and returns the docker CLI's extra environment (the headers, passed to
// the container by name so the value never appears in the docker argv).
func withTelemetryDocker(ctx context.Context, t *CLITelemetry, spec *containerSpec) (dockerEnv []string) {
	if t == nil {
		return nil
	}
	spec.SetEnv = append(spec.SetEnv, t.Env(ctx)...)
	spec.SetEnv = withNoProxy(spec.SetEnv, t.NoProxyHost())
	if kv, ok := t.SecretEnv(); ok {
		spec.PassEnv = append(spec.PassEnv, HeadersEnv)
		dockerEnv = append(dockerEnv, kv)
	}
	return dockerEnv
}

// withTelemetryLocal adds the CLI telemetry to a local-mode environment.
func withTelemetryLocal(ctx context.Context, t *CLITelemetry, env []string) []string {
	if t == nil {
		return env
	}
	env = append(env, t.Env(ctx)...)
	if kv, ok := t.SecretEnv(); ok {
		env = append(env, kv)
	}
	return withNoProxy(env, t.NoProxyHost())
}

// filterEnv drops the variables named in hide (case-insensitive).
func filterEnv(env, hide []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		hidden := slices.ContainsFunc(hide, func(h string) bool { return strings.EqualFold(h, name) })
		if !hidden {
			out = append(out, kv)
		}
	}
	return out
}
