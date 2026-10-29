package claude

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// An error result without subtype is reported as "error"; a failure with
// no output at all is described by the exec error.
func TestParseOutputErrorWithoutSubtype(t *testing.T) {
	_, err := parseOutput(processOutput{Stdout: []byte(`{"type":"result","is_error":true,"result":"boom"}`)}, false)
	if err == nil || err.Error() != "claude run failed (error): boom" {
		t.Fatalf("err = %v", err)
	}
	_, err = parseOutput(processOutput{ExitCode: -1, Err: errors.New("fork/exec claude: no such file")}, false)
	if err == nil || err.Error() != "claude exited with code -1: fork/exec claude: no such file" {
		t.Fatalf("err = %v", err)
	}
	if got := describeFailure(processOutput{}); got != "" {
		t.Fatalf("describeFailure(empty) = %q", got)
	}
}

// extractJSON: text that starts like an object but is not JSON yields no
// object; a valid object later in the text is still found.
func TestExtractJSONInvalidObject(t *testing.T) {
	if j, ok := extractJSON("{not json"); ok {
		t.Errorf("got %q", j)
	}
	if j, ok := extractJSON(`{broken} then {"a": 1}`); !ok || j != `{"a":1}` {
		t.Errorf("got %q %v", j, ok)
	}
}

// Rate-limit detection works line by line: the message is the matching
// line, text without such a line is no rate limit.
func TestDetectRateLimitLines(t *testing.T) {
	if rl := detectRateLimit("all good\nnothing here"); rl != nil {
		t.Fatalf("false positive: %+v", rl)
	}
	rl := detectRateLimit("starting\n  Claude AI usage limit reached|1735689600  \ntrailer")
	if rl == nil || rl.Message != "Claude AI usage limit reached|1735689600" || rl.ResetAt.Unix() != 1735689600 {
		t.Fatalf("rl = %+v", rl)
	}
	long := strings.Repeat("x", 600) + " rate limit"
	if rl := detectRateLimit(long); rl == nil || rl.Message != "..."+long[len(long)-500:] {
		t.Fatalf("long line: %+v", rl)
	}
}

// Read paths are deduplicated and capped; blank paths are ignored.
func TestStreamReadFilesCap(t *testing.T) {
	sc := newStreamCapture(context.Background())
	sc.addRead("  ")
	for i := 0; i < maxReadFiles+10; i++ {
		sc.addRead(fmt.Sprintf("/work/f%d.go", i))
		sc.addRead(fmt.Sprintf("/work/f%d.go", i))
	}
	got := sc.ReadFiles()
	if len(got) != maxReadFiles || got[0] != "/work/f0.go" || got[maxReadFiles-1] != fmt.Sprintf("/work/f%d.go", maxReadFiles-1) {
		t.Fatalf("reads = %d, first %q", len(got), got[0])
	}
}

// Non-event output kept for error reporting is bounded.
func TestStreamRawOutputCap(t *testing.T) {
	sc := newStreamCapture(context.Background())
	line := strings.Repeat("n", 1000) + "\n"
	for i := 0; i < 2*maxStreamRaw/len(line); i++ {
		sc.Write([]byte(line))
	}
	if n := len(sc.Bytes()); n > maxStreamRaw || n < maxStreamRaw-len(line) {
		t.Fatalf("raw = %d bytes", n)
	}
}

// refineError keeps an error that already is a rate limit.
func TestRefineErrorKeepsRateLimit(t *testing.T) {
	sc := newStreamCapture(context.Background())
	sc.Write([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1735689600}}` + "\n"))
	orig := &core.ErrClaudeRateLimited{Message: "usage limit reached"}
	if got := sc.refineError(orig); got != error(orig) {
		t.Fatalf("got %v", got)
	}
	wrapped := fmt.Errorf("run: %w", orig)
	if got := sc.refineError(wrapped); got != wrapped {
		t.Fatalf("got %v", got)
	}
}

// A plain failure of a run whose stream reported a rejected rate limit
// becomes a rate-limit error with the reset time.
func TestRefineErrorPlainFailure(t *testing.T) {
	sc := newStreamCapture(context.Background())
	sc.Write([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1735689600}}` + "\n"))
	var rl *core.ErrClaudeRateLimited
	if err := sc.refineError(errors.New("claude exited with code 1")); !errors.As(err, &rl) ||
		rl.Message != "Claude usage limit reached (five_hour)|1735689600" || rl.ResetAt.Unix() != 1735689600 {
		t.Fatalf("err = %v", err)
	}
	if err := newStreamCapture(context.Background()).refineError(errors.New("x")); err.Error() != "x" {
		t.Fatalf("without a rejection: %v", err)
	}
}

// The observer gets the tail of long process output.
func TestObserveProcessOutputTail(t *testing.T) {
	obs := &recObserver{}
	ctx := WithObserver(context.Background(), obs)
	out := strings.Repeat("a", ProcessTailBytes) + "END"
	observeProcess(ctx, exec.Command("x"), processOutput{Stdout: []byte(out), Stderr: []byte("short")}, 0)
	if len(obs.procs) != 1 || obs.procs[0].Stdout != out[3:] || obs.procs[0].Stderr != "short" || obs.procs[0].Err != "" {
		t.Fatalf("procs = %+v", obs.procs)
	}
}

// A process that could not start is reported to the observer with its
// error.
func TestObserveProcessStartError(t *testing.T) {
	obs := &recObserver{}
	ctx := WithObserver(context.Background(), obs)
	r := &runner{}
	cmd := exec.CommandContext(ctx, "/nonexistent/claude", "-p")
	p := r.execWith(ctx, cmd, "", &capBuffer{max: 10})
	if p.ExitCode != -1 || p.Err == nil {
		t.Fatalf("p = %+v", p)
	}
	if len(obs.procs) != 1 || obs.procs[0].Err == "" || obs.procs[0].ExitCode != -1 || !slices.Equal(obs.procs[0].Argv, []string{"/nonexistent/claude", "-p"}) {
		t.Fatalf("procs = %+v", obs.procs)
	}
}

// Without telemetry nothing changes; filterEnv without names keeps all.
func TestTelemetryNilAndFilterEnv(t *testing.T) {
	var tel *CLITelemetry
	if tel.NoProxyHost() != "" {
		t.Error("nil NoProxyHost")
	}
	env := []string{"A=1", "NO_PROXY=x"}
	if got := withTelemetryLocal(context.Background(), nil, env); !slices.Equal(got, env) {
		t.Errorf("withTelemetryLocal(nil) = %v", got)
	}
	tel = NewCLITelemetry(CLITelemetryOptions{Endpoint: "http://otel:4318", NoProxyHost: "otel", Headers: map[string]string{"Authorization": "Bearer s"}})
	got := withTelemetryLocal(context.Background(), tel, []string{"A=1"})
	if !slices.Contains(got, HeadersEnv+"=Authorization=Bearer%20s") || !slices.Contains(got, "NO_PROXY=localhost,127.0.0.1,otel") {
		t.Errorf("withTelemetryLocal = %v", got)
	}
	if got := filterEnv(env, nil); !slices.Equal(got, env) {
		t.Errorf("filterEnv(nil) = %v", got)
	}
	if got := filterEnv([]string{"Secret=1", "B=2", "SECRET=3"}, []string{"secret"}); !slices.Equal(got, []string{"B=2"}) {
		t.Errorf("filterEnv = %v", got)
	}
}

// A modelUsage map with only zero counters does not hide the top-level
// usage.
func TestUsageZeroModelUsageFallsBack(t *testing.T) {
	r := &cliResult{
		Usage:      &cliUsage{InputTokens: 7, OutputTokens: 3},
		ModelUsage: map[string]cliModelUsage{"claude-opus": {CostUSD: 0.1}},
	}
	u, models := r.usage()
	if u.Input != 7 || u.Output != 3 || len(models) != 1 || models["claude-opus"].CostUSD != 0.1 {
		t.Fatalf("usage = %+v models = %+v", u, models)
	}
}

// A rejected token is not an "internal error": the CLI exits at once with a
// 401 result, which becomes *core.ErrClaudeAuth.
func TestParseOutputAuthFailure(t *testing.T) {
	out := `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Failed to authenticate. API Error: 401 OAuth access token is invalid."}`
	_, err := parseOutput(processOutput{ExitCode: 1, Stdout: []byte(out)}, false)
	var ae *core.ErrClaudeAuth
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "401") {
		t.Fatalf("err = %v", err)
	}
	// The same words in the model's own successful answer are no failure.
	ok := `{"type":"result","subtype":"success","is_error":false,"result":"Failed to authenticate the user: 401"}`
	if _, err := parseOutput(processOutput{Stdout: []byte(ok)}, false); err != nil {
		t.Fatalf("err = %v", err)
	}
	// Other API errors stay ordinary failures.
	other := `{"type":"result","subtype":"success","is_error":true,"api_error_status":500,"result":"Internal server error"}`
	_, err = parseOutput(processOutput{ExitCode: 1, Stdout: []byte(other)}, false)
	if err == nil || errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
}
