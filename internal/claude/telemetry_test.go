package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

func TestCLITelemetryEnv(t *testing.T) {
	if NewCLITelemetry(CLITelemetryOptions{Endpoint: "not a url"}) != nil {
		t.Fatal("invalid endpoint accepted")
	}
	tel := NewCLITelemetry(CLITelemetryOptions{Endpoint: "http://tempo:4318", Environment: "prod env", NoProxyHost: "tempo"})
	tr := tracing.New(tracing.Options{})
	ctx, sp := tr.Start(context.Background(), "claude review")
	env := strings.Join(tel.Env(ctx), "\n")
	for _, want := range []string{"CLAUDE_CODE_ENABLE_TELEMETRY=1", "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1", "OTEL_TRACES_EXPORTER=otlp",
		"OTEL_METRICS_EXPORTER=none", "OTEL_LOGS_EXPORTER=none", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/json",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://tempo:4318/v1/traces", "deployment.environment=prod+env",
		"TRACEPARENT=" + sp.SpanContext().TraceParent()} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %q in\n%s", want, env)
		}
	}
	for _, bad := range []string{"OTEL_LOG_USER_PROMPTS", "OTEL_LOG_TOOL", "HEADERS"} {
		if strings.Contains(env, bad) {
			t.Errorf("%s must not be set", bad)
		}
	}
	if _, ok := tel.SecretEnv(); ok {
		t.Error("headers without claude_cli_headers")
	}
	// No span in ctx: no TRACEPARENT.
	if strings.Contains(strings.Join(tel.Env(context.Background()), "\n"), "TRACEPARENT") {
		t.Error("TRACEPARENT without a span")
	}
	var nilTel *CLITelemetry
	if nilTel.Env(ctx) != nil {
		t.Error("nil telemetry has env")
	}
	withH := NewCLITelemetry(CLITelemetryOptions{Endpoint: "https://otlp.example", Headers: map[string]string{"Authorization": "Bearer a b"}})
	if kv, ok := withH.SecretEnv(); !ok || kv != "OTEL_EXPORTER_OTLP_TRACES_HEADERS=Authorization=Bearer%20a%20b" {
		t.Errorf("secret env %q", kv)
	}
	if !core.IsServiceSecretEnv(HeadersEnv) || !core.IsServiceSecretEnv("OTEL_EXPORTER_OTLP_HEADERS") {
		t.Error("header variables must be service secrets")
	}
}

// TestRunDockerTelemetry: with the flag on the container gets the
// telemetry env (headers by name only, never in argv) and the collector in
// NO_PROXY; with it off, none of it.
func TestRunDockerTelemetry(t *testing.T) {
	for _, on := range []bool{true, false} {
		bin := t.TempDir()
		logf := filepath.Join(bin, "log")
		docker := writeScript(t, bin, "docker", `
echo "ARGS: $*" >> "`+logf+`"
echo "HDR=$OTEL_EXPORTER_OTLP_TRACES_HEADERS TT=$TEMPO_TOKEN" >> "`+logf+`"
cat >/dev/null
echo '`+fakeResult+`'
`)
		o := dockerOpts()
		o.DockerBin = docker
		o.HideEnv = []string{"TEMPO_TOKEN"}
		if on {
			o.Telemetry = NewCLITelemetry(CLITelemetryOptions{Endpoint: "http://tempo:4318", NoProxyHost: "tempo",
				Headers: map[string]string{"Authorization": "Bearer tempo-secret"}})
		}
		r, err := newRunner(o, func() []string {
			return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k", "TEMPO_TOKEN=tempo-secret", "OTEL_EXPORTER_OTLP_HEADERS=x=y"}
		})
		if err != nil {
			t.Fatal(err)
		}
		tr := tracing.New(tracing.Options{})
		ctx, sp := tr.Start(context.Background(), "claude review")
		if _, err := r.Run(ctx, core.RunRequest{Dir: t.TempDir(), Prompt: "x"}); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(logf)
		log := string(b)
		args := strings.SplitN(log, "\n", 2)[0]
		if strings.Contains(args, "tempo-secret") || strings.Contains(log, "TT=tempo-secret") {
			t.Errorf("on=%v: secret leaked:\n%s", on, log)
		}
		has := strings.Contains(args, "-e CLAUDE_CODE_ENABLE_TELEMETRY=1") && strings.Contains(args, "-e TRACEPARENT="+sp.SpanContext().TraceParent()) &&
			strings.Contains(args, "-e OTEL_EXPORTER_OTLP_TRACES_HEADERS ") && strings.Contains(log, "HDR=Authorization=Bearer%20tempo-secret") &&
			strings.Contains(args, "NO_PROXY=localhost,127.0.0.1,tempo")
		if has != on {
			t.Errorf("on=%v: telemetry env present=%v:\n%s", on, has, log)
		}
		if !on && (strings.Contains(args, "OTEL_") || strings.Contains(args, "TRACEPARENT") || strings.Contains(log, "HDR=x=y")) {
			t.Errorf("off: OTel env reached the container:\n%s", log)
		}
	}
}

func TestRunLocalTelemetry(t *testing.T) {
	bin := t.TempDir()
	claudeBin := writeScript(t, bin, "claude", `
echo "ENABLE=$CLAUDE_CODE_ENABLE_TELEMETRY TP=$TRACEPARENT TT=$TEMPO_TOKEN H=$OTEL_EXPORTER_OTLP_HEADERS" > env.txt
cat > /dev/null
echo '`+fakeResult+`'
`)
	work := t.TempDir()
	o := Options{Mode: "local", ClaudeBin: claudeBin, HideEnv: []string{"TEMPO_TOKEN"},
		Telemetry: NewCLITelemetry(CLITelemetryOptions{Endpoint: "http://localhost:4318"})}
	r, err := newRunner(o, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k", "TEMPO_TOKEN=tt", "OTEL_EXPORTER_OTLP_HEADERS=a=b"}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, sp := tracing.New(tracing.Options{}).Start(context.Background(), "claude review")
	if _, err := r.Run(ctx, core.RunRequest{Dir: work, Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(work, "env.txt"))
	if got, want := strings.TrimSpace(string(b)), "ENABLE=1 TP="+sp.SpanContext().TraceParent()+" TT= H="; got != want {
		t.Fatalf("local env = %q, want %q", got, want)
	}
}

// The collector URL may carry user:password@ (basic auth of the bot's own
// exporter). It never reaches the CLI's endpoint variable, which is a
// literal `docker run -e NAME=value` (visible in ps). With
// tracing.claude_cli_headers the credentials go as an Authorization: Basic
// header through the secret headers variable (by name); without it, not at
// all.
func TestCLITelemetryEndpointUserinfo(t *testing.T) {
	const ep = "https://bot:p%40ss-secret@otlp.example:4318/otlp"
	for _, pass := range []bool{false, true} {
		tel := NewCLITelemetry(CLITelemetryOptions{Endpoint: ep, PassCredentials: pass})
		env := strings.Join(tel.Env(context.Background()), "\n")
		if !strings.Contains(env, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://otlp.example:4318/otlp/v1/traces\n") ||
			strings.Contains(env, "bot:") || strings.Contains(env, "@otlp") || strings.Contains(env, "ss-secret") {
			t.Errorf("pass=%v: endpoint env:\n%s", pass, env)
		}
		kv, ok := tel.SecretEnv()
		// base64("bot:p@ss-secret")
		want := HeadersEnv + "=Authorization=Basic%20Ym90OnBAc3Mtc2VjcmV0"
		if ok != pass || (pass && kv != want) {
			t.Errorf("pass=%v: secret env %q %v, want %q", pass, kv, ok, want)
		}
	}
	// An explicit Authorization header wins over the URL's credentials.
	tel := NewCLITelemetry(CLITelemetryOptions{Endpoint: ep, PassCredentials: true, Headers: map[string]string{"authorization": "Bearer t", "X-Scope-OrgID": "a"}})
	if kv, _ := tel.SecretEnv(); kv != HeadersEnv+"=X-Scope-OrgID=a,authorization=Bearer%20t" {
		t.Errorf("explicit header: %q", kv)
	}
}

// Docker mode: the password from the collector URL is nowhere in the
// docker argv.
func TestRunDockerTelemetryUserinfoNotInArgv(t *testing.T) {
	bin := t.TempDir()
	logf := filepath.Join(bin, "log")
	docker := writeScript(t, bin, "docker", `
echo "ARGS: $*" >> "`+logf+`"
echo "HDR=$OTEL_EXPORTER_OTLP_TRACES_HEADERS" >> "`+logf+`"
cat >/dev/null
echo '`+fakeResult+`'
`)
	for _, pass := range []bool{false, true} {
		_ = os.Remove(logf)
		o := dockerOpts()
		o.DockerBin = docker
		o.Telemetry = NewCLITelemetry(CLITelemetryOptions{Endpoint: "http://bot:tempo-pw@tempo:4318", NoProxyHost: "tempo", PassCredentials: pass})
		r, err := newRunner(o, func() []string { return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"} })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Run(context.Background(), core.RunRequest{Dir: t.TempDir(), Prompt: "x"}); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(logf)
		args, hdr, _ := strings.Cut(string(b), "\n")
		if strings.Contains(args, "tempo-pw") || strings.Contains(args, "bot:") ||
			!strings.Contains(args, "-e OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://tempo:4318/v1/traces ") {
			t.Errorf("pass=%v: argv:\n%s", pass, args)
		}
		if got := strings.Contains(hdr, "HDR=Authorization=Basic%20"); got != pass {
			t.Errorf("pass=%v: header %q", pass, hdr)
		}
	}
}
