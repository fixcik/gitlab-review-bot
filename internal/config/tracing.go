package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Tracing configures OpenTelemetry traces of the bot (internal/tracing):
// one trace per queue job, exported over OTLP/HTTP (JSON) to Tempo, an
// OpenTelemetry Collector, Grafana Alloy or Grafana Cloud.
//
// Environment overrides (the standard OTel variables; env wins over the
// yaml, like GITLAB_URL does): OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, then
// OTEL_EXPORTER_OTLP_ENDPOINT -> endpoint; OTEL_EXPORTER_OTLP_HEADERS
// (k=v,k2=v2, URL-encoded values) -> merged over headers, per key;
// OTEL_SERVICE_NAME -> service_name; OTEL_SDK_DISABLED=true -> off. The
// variables never turn tracing on by themselves: enabled must be true.
type Tracing struct {
	// Enabled turns tracing on (default false).
	Enabled bool `yaml:"enabled"`
	// Endpoint is the OTLP/HTTP base URL ("http://tempo:4318");
	// /v1/traces is appended unless present.
	Endpoint string `yaml:"endpoint"`
	// Headers are sent with every export request (Authorization for
	// Grafana Cloud, X-Scope-OrgID for multi-tenant Tempo). A value may
	// reference environment variables as ${NAME}; keep secrets there.
	// Values are never logged and are scrubbed from diagnostics.
	Headers map[string]string `yaml:"headers"`
	// ServiceName is service.name (default "gitlab-review-bot").
	ServiceName string `yaml:"service_name"`
	// Environment is deployment.environment ("" = not set).
	Environment string `yaml:"environment"`
	// SampleRatio is the share of jobs (and webhook/poll traces) kept, 0..1
	// (default 1). Head sampling: decided when the trace starts.
	SampleRatio *float64 `yaml:"sample_ratio"`
	// Timeout bounds one export request (default 10s).
	Timeout Duration `yaml:"timeout"`
	// UIURL builds the trace link on /runs: "{trace_id}" is replaced by the
	// hex trace id (e.g. a Grafana Explore URL).
	UIURL string `yaml:"ui_url"`
	// CAFile adds PEM certificates for an https collector with a private CA.
	CAFile string `yaml:"ca_file"`
	// ClaudeCLI (experimental, default false) turns on Claude Code's own
	// OpenTelemetry traces inside the sandbox (its spans join the bot's
	// trace through TRACEPARENT). The sandbox then talks to the collector:
	// see docs/features/tracing.md.
	ClaudeCLI bool `yaml:"claude_cli"`
	// ClaudeCLIEndpoint is the collector as seen from the sandbox network
	// (default: endpoint). An https endpoint on port 443 goes through the
	// egress proxy (its host is added to the allowlist); an http one must be
	// reachable directly from the sandbox network.
	ClaudeCLIEndpoint string `yaml:"claude_cli_endpoint"`
	// ClaudeCLIHeaders passes headers to the CLI too (default false: the
	// exporter credentials never enter the sandbox unless asked).
	ClaudeCLIHeaders bool `yaml:"claude_cli_headers"`

	// headerEnv are the variables referenced by headers (${NAME}).
	headerEnv []string
	// headerErrs: unset variables referenced by headers.
	headerErrs []string
}

// Tracing defaults.
const (
	DefaultTracingService = "gitlab-review-bot"
	DefaultTracingTimeout = 10 * time.Second
)

func defaultTracing() Tracing {
	return Tracing{ServiceName: DefaultTracingService, Timeout: Duration(DefaultTracingTimeout)}
}

// On reports whether tracing is enabled.
func (t Tracing) On() bool { return t.Enabled }

// Ratio is sample_ratio (default 1).
func (t Tracing) Ratio() float64 {
	if t.SampleRatio == nil {
		return 1
	}
	return *t.SampleRatio
}

// TimeoutD is timeout (default 10s).
func (t Tracing) TimeoutD() time.Duration { return positiveOr(t.Timeout.D(), DefaultTracingTimeout) }

// HeaderEnv returns the names of the environment variables the headers
// reference: their values are secrets (scrubbed, kept from the CLI).
func (t Tracing) HeaderEnv() []string { return append([]string(nil), t.headerEnv...) }

// HeaderValues returns the resolved header values (secrets to scrub).
func (t Tracing) HeaderValues() []string {
	var out []string
	for _, v := range t.Headers {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
			// "Bearer <token>" / "Basic <b64>": the credential alone too.
			if _, cred, ok := strings.Cut(v, " "); ok && strings.TrimSpace(cred) != "" {
				out = append(out, strings.TrimSpace(cred))
			}
		}
	}
	sort.Strings(out)
	return out
}

// CLIEndpoint is the collector URL for the Claude CLI.
func (t Tracing) CLIEndpoint() string {
	if t.ClaudeCLIEndpoint != "" {
		return t.ClaudeCLIEndpoint
	}
	return t.Endpoint
}

// CLIEgressHost is the collector host the egress proxy must let through
// for the Claude CLI: set only with claude_cli on and an https endpoint
// on port 443 (the proxy only tunnels CONNECT :443). "" otherwise.
func (t Tracing) CLIEgressHost() string {
	if !t.Enabled || !t.ClaudeCLI {
		return ""
	}
	u, err := url.Parse(t.CLIEndpoint())
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || (u.Port() != "" && u.Port() != "443") {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// CLIDirectHost is the collector host the Claude CLI reaches directly (an
// http endpoint, or https on another port): it goes into NO_PROXY and the
// collector must be on the sandbox network. "" otherwise.
func (t Tracing) CLIDirectHost() string {
	if !t.Enabled || !t.ClaudeCLI || t.CLIEgressHost() != "" {
		return ""
	}
	u, err := url.Parse(t.CLIEndpoint())
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// headerNameRe is an HTTP token (RFC 9110 field-name).
var headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// applyTracingEnv applies the OTel environment variables and resolves
// ${NAME} references in headers.
func applyTracingEnv(t *Tracing) {
	if v := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); v != "" {
		t.Endpoint = v
	}
	if v := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")); v != "" {
		t.Endpoint = v
	}
	if v := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); v != "" {
		t.ServiceName = v
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		t.Enabled = false
	}
	t.Endpoint = strings.TrimSpace(t.Endpoint)
	t.ClaudeCLIEndpoint = strings.TrimSpace(t.ClaudeCLIEndpoint)
	t.ServiceName = strings.TrimSpace(t.ServiceName)
	if t.ServiceName == "" {
		t.ServiceName = DefaultTracingService
	}
	t.UIURL = strings.TrimSpace(t.UIURL)
	// ${NAME} references in the yaml headers.
	t.headerEnv, t.headerErrs = nil, nil
	resolved := map[string]string{}
	keys := make([]string, 0, len(t.Headers))
	for k := range t.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := envRefRe.ReplaceAllStringFunc(t.Headers[k], func(ref string) string {
			name := envRefRe.FindStringSubmatch(ref)[1]
			t.headerEnv = append(t.headerEnv, name)
			val, ok := os.LookupEnv(name)
			if !ok || val == "" {
				t.headerErrs = append(t.headerErrs, fmt.Sprintf("tracing.headers.%s references ${%s}, which is not set", k, name))
			}
			return val
		})
		resolved[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	for _, kv := range strings.Split(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"), ",") {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		if dv, err := url.QueryUnescape(strings.TrimSpace(v)); err == nil {
			v = dv
		}
		resolved[k] = strings.TrimSpace(v)
	}
	if len(resolved) > 0 {
		t.Headers = resolved
	}
}

func (t Tracing) validate(sandboxMode string) []string {
	if !t.Enabled {
		if t.ClaudeCLI {
			return []string{"tracing.claude_cli requires tracing.enabled: true"}
		}
		return nil
	}
	var errs []string
	if t.Endpoint == "" {
		errs = append(errs, "tracing.endpoint (or OTEL_EXPORTER_OTLP_ENDPOINT) is required with tracing.enabled")
	} else if !validHTTPURL(t.Endpoint) {
		errs = append(errs, "tracing.endpoint must be an http(s) URL")
	}
	if t.SampleRatio != nil && (*t.SampleRatio < 0 || *t.SampleRatio > 1 || *t.SampleRatio != *t.SampleRatio) {
		errs = append(errs, "tracing.sample_ratio must be between 0 and 1")
	}
	if t.Timeout < 0 || t.Timeout.D() > 2*time.Minute {
		errs = append(errs, "tracing.timeout must be between 0 and 2m")
	}
	if t.UIURL != "" && (!validHTTPURL(strings.ReplaceAll(t.UIURL, "{trace_id}", "x")) || !strings.Contains(t.UIURL, "{trace_id}")) {
		errs = append(errs, "tracing.ui_url must be an http(s) URL containing {trace_id}")
	}
	for k := range t.Headers {
		if !headerNameRe.MatchString(k) {
			errs = append(errs, fmt.Sprintf("tracing.headers: %q is not a valid header name", k))
		}
	}
	errs = append(errs, t.headerErrs...)
	if t.ClaudeCLIEndpoint != "" && !validHTTPURL(t.ClaudeCLIEndpoint) {
		errs = append(errs, "tracing.claude_cli_endpoint must be an http(s) URL")
	}
	if t.ClaudeCLIHeaders && !t.ClaudeCLI {
		errs = append(errs, "tracing.claude_cli_headers requires tracing.claude_cli: true")
	}
	if t.ClaudeCLI && sandboxMode == "docker" {
		if u, err := url.Parse(t.CLIEndpoint()); err == nil && (strings.EqualFold(u.Hostname(), "localhost") || isLoopback(u.Hostname())) {
			errs = append(errs, "tracing.claude_cli: the collector endpoint is a loopback address, which the sandbox container cannot reach; set tracing.claude_cli_endpoint")
		}
	}
	return errs
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
