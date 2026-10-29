package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func tracingEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITLAB_URL", "https://g.example")
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_SERVICE_NAME", "OTEL_SDK_DISABLED", "TEMPO_TOKEN"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTracingDefaults(t *testing.T) {
	tracingEnv(t)
	cfg, err := LoadService("")
	if err != nil {
		t.Fatal(err)
	}
	tr := cfg.Tracing
	if tr.On() || tr.ServiceName != "gitlab-review-bot" || tr.Ratio() != 1 || tr.TimeoutD() != DefaultTracingTimeout || tr.ClaudeCLI {
		t.Fatalf("defaults: %+v", tr)
	}
	// Disabled: nothing is validated.
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTracingFileAndEnvPrecedence(t *testing.T) {
	tracingEnv(t)
	t.Setenv("TEMPO_TOKEN", "s3cret-token")
	p := writeCfg(t, `
tracing:
  enabled: true
  endpoint: http://tempo:4318
  service_name: bot-a
  environment: prod
  sample_ratio: 0.25
  timeout: 3s
  ui_url: https://grafana.example/explore?traceId={trace_id}
  headers:
    Authorization: "Bearer ${TEMPO_TOKEN}"
    X-Scope-OrgID: team
`)
	cfg, err := LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	tr := cfg.Tracing
	if !tr.On() || tr.Endpoint != "http://tempo:4318" || tr.ServiceName != "bot-a" || tr.Environment != "prod" || tr.Ratio() != 0.25 ||
		tr.TimeoutD().String() != "3s" {
		t.Fatalf("file: %+v", tr)
	}
	if tr.Headers["Authorization"] != "Bearer s3cret-token" || tr.Headers["X-Scope-OrgID"] != "team" {
		t.Fatalf("headers: %v", tr.Headers)
	}
	if !slices.Equal(tr.HeaderEnv(), []string{"TEMPO_TOKEN"}) {
		t.Fatalf("header env = %v", tr.HeaderEnv())
	}
	if vals := tr.HeaderValues(); !slices.Contains(vals, "s3cret-token") || !slices.Contains(vals, "Bearer s3cret-token") {
		t.Fatalf("header values for scrubbing = %v", vals)
	}

	// Standard OTel variables override the file.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_SERVICE_NAME", "bot-env")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "X-Scope-OrgID=other,api-key=a%20b")
	cfg, err = LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	tr = cfg.Tracing
	if tr.Endpoint != "http://collector:4318" || tr.ServiceName != "bot-env" {
		t.Fatalf("env override: %+v", tr)
	}
	if tr.Headers["X-Scope-OrgID"] != "other" || tr.Headers["api-key"] != "a b" || tr.Headers["Authorization"] != "Bearer s3cret-token" {
		t.Fatalf("merged headers: %v", tr.Headers)
	}
	// The signal-specific endpoint wins over the generic one.
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://otlp.example/v1/traces")
	if cfg, _ = LoadService(p); cfg.Tracing.Endpoint != "https://otlp.example/v1/traces" {
		t.Fatalf("traces endpoint: %q", cfg.Tracing.Endpoint)
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if cfg, _ = LoadService(p); cfg.Tracing.On() {
		t.Fatal("OTEL_SDK_DISABLED ignored")
	}
}

func TestTracingEnvDoesNotEnable(t *testing.T) {
	tracingEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	cfg, err := LoadService("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tracing.On() {
		t.Fatal("an OTel env variable alone turned tracing on")
	}
}

func TestTracingValidation(t *testing.T) {
	tracingEnv(t)
	cases := map[string]string{
		"enabled: true":                         "tracing.endpoint",
		"enabled: true\n  endpoint: tempo:4318": "must be an http(s) URL",
		"enabled: true\n  endpoint: http://t:4318\n  sample_ratio: 1.5":                                      "sample_ratio",
		"enabled: true\n  endpoint: http://t:4318\n  sample_ratio: -0.1":                                     "sample_ratio",
		"enabled: true\n  endpoint: http://t:4318\n  timeout: 10m":                                           "tracing.timeout",
		"enabled: true\n  endpoint: http://t:4318\n  ui_url: https://g/x":                                    "{trace_id}",
		"enabled: true\n  endpoint: http://t:4318\n  ui_url: javascript:alert({trace_id})":                   "ui_url",
		"enabled: true\n  endpoint: http://t:4318\n  headers:\n    \"Bad Header\": x":                        "valid header name",
		"enabled: true\n  endpoint: http://t:4318\n  headers:\n    Authorization: \"Bearer ${TEMPO_TOKEN}\"": "${TEMPO_TOKEN}",
		"claude_cli: true": "requires tracing.enabled",
		"enabled: true\n  endpoint: http://t:4318\n  claude_cli_headers: true":                      "claude_cli_headers requires",
		"enabled: true\n  endpoint: http://127.0.0.1:4318\n  claude_cli: true":                      "loopback",
		"enabled: true\n  endpoint: http://t:4318\n  claude_cli: true\n  claude_cli_endpoint: nope": "claude_cli_endpoint",
	}
	for body, want := range cases {
		_, err := LoadService(writeCfg(t, "tracing:\n  "+body+"\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
	ok := "tracing:\n  enabled: true\n  endpoint: http://localhost:4318\n  sample_ratio: 0\n  ui_url: http://g/{trace_id}\n"
	if _, err := LoadService(writeCfg(t, ok)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	// Loopback is fine for the CLI in local mode (it runs on the host).
	local := "sandbox:\n  mode: local\ntracing:\n  enabled: true\n  endpoint: http://localhost:4318\n  claude_cli: true\n"
	if _, err := LoadService(writeCfg(t, local)); err != nil {
		t.Fatalf("local mode loopback rejected: %v", err)
	}
}

func TestTracingCLIEgress(t *testing.T) {
	tracingEnv(t)
	cases := []struct {
		yaml           string
		egress, direct string
	}{
		{"enabled: true\n  endpoint: http://tempo:4318", "", ""}, // claude_cli off
		{"enabled: true\n  endpoint: http://tempo:4318\n  claude_cli: true", "", "tempo"},
		{"enabled: true\n  endpoint: http://tempo:4318\n  claude_cli: true\n  claude_cli_endpoint: https://OTLP.example.com/otlp", "otlp.example.com", ""},
		{"enabled: true\n  endpoint: https://otlp.example.com:4318\n  claude_cli: true", "", "otlp.example.com"},
	}
	for _, c := range cases {
		p := writeCfg(t, "tracing:\n  "+c.yaml+"\n")
		cfg, err := LoadService(p)
		if err != nil {
			t.Fatalf("%q: %v", c.yaml, err)
		}
		if got := cfg.Tracing.CLIEgressHost(); got != c.egress {
			t.Errorf("%q: egress host = %q, want %q", c.yaml, got, c.egress)
		}
		if got := cfg.Tracing.CLIDirectHost(); got != c.direct {
			t.Errorf("%q: direct host = %q, want %q", c.yaml, got, c.direct)
		}
		sb, err := LoadSandbox(p)
		if err != nil {
			t.Fatal(err)
		}
		if has := slices.Contains(sb.EgressAllow, c.egress); has != (c.egress != "") {
			t.Errorf("%q: egress allowlist = %v", c.yaml, sb.EgressAllow)
		}
		if len(sb.EgressAllow) != len(Defaults().Sandbox.EgressAllow)+map[bool]int{true: 1, false: 0}[c.egress != ""] {
			t.Errorf("%q: allowlist grew unexpectedly: %v", c.yaml, sb.EgressAllow)
		}
	}
}
