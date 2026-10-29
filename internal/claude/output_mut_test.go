package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// The advisor fallback (retry once without --advisor, then never use it
// again) is only for advisor refusals of runs that asked for the advisor.
func TestAdvisorFallbackOnlyForAdvisorErrors(t *testing.T) {
	bin := t.TempDir()
	claudeBin := writeScript(t, bin, "claude", `
echo x >> calls.txt
cat > /dev/null
echo "{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"result\":\"$(cat msg.txt)\"}"
exit 1
`)
	r, err := newRunner(Options{Mode: "local", ClaudeBin: claudeBin}, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, msg, advisor string
	}{
		{"other failure with advisor", "boom", "opus"},
		{"advisor text without advisor", "the advisor tool is not available", ""},
	} {
		work := t.TempDir()
		os.WriteFile(filepath.Join(work, "msg.txt"), []byte(tc.msg), 0o644)
		_, err := r.Run(context.Background(), core.RunRequest{Dir: work, Prompt: "hi", Advisor: tc.advisor})
		if err == nil {
			t.Fatalf("%s: want error", tc.name)
		}
		b, _ := os.ReadFile(filepath.Join(work, "calls.txt"))
		if n := strings.Count(string(b), "x"); n != 1 || r.advisorOff.Load() {
			t.Fatalf("%s: calls %d, advisorOff %v", tc.name, n, r.advisorOff.Load())
		}
	}
}

func TestIsAdvisorErrorIgnoresCancellation(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("advisor run aborted: %w", context.Canceled),
		fmt.Errorf("advisor run aborted: %w", context.DeadlineExceeded),
		&core.FailedRun{Err: &core.ErrClaudeRateLimited{Message: "advisor usage limit reached"}},
		nil,
	} {
		if isAdvisorError(err) {
			t.Errorf("isAdvisorError(%v) = true", err)
		}
	}
	if !isAdvisorError(errors.New("claude run failed: The configured advisor model is not compatible")) {
		t.Error("advisor refusal not recognized")
	}
}

// An error result keeps its own text; the errors list is only a fallback.
func TestErrorResultMessage(t *testing.T) {
	_, err := parseOutput(processOutput{Stdout: []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"main text","errors":["side"]}`), ExitCode: 1}, false)
	if err == nil || !strings.Contains(err.Error(), "main text") || strings.Contains(err.Error(), "side") {
		t.Fatalf("err = %v", err)
	}
	_, err = parseOutput(processOutput{Stdout: []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","errors":["e1","e2"]}`), ExitCode: 1}, false)
	if err == nil || !strings.Contains(err.Error(), "e1; e2") {
		t.Fatalf("err = %v", err)
	}
}

// decodeResult's defensive fallbacks pick the last "result" object even when
// other events or noise follow it, in line-delimited and array form.
func TestDecodeResultFallbacks(t *testing.T) {
	for name, out := range map[string]string{
		"lines": `noise
{"type":"result","subtype":"success","result":"R"}
{"type":"system","subtype":"after"}
trailing noise`,
		"array": `[{"type":"system"},{"type":"result","subtype":"success","result":"R"},{"type":"assistant"}]`,
	} {
		r, err := decodeResult([]byte(out))
		if err != nil || r.Type != "result" || r.Result != "R" {
			t.Errorf("%s: %+v %v", name, r, err)
		}
	}
}

func TestDetectRateLimitTrailingTimestamp(t *testing.T) {
	rl := detectRateLimit("Rate limit exceeded, try again at 1735689600")
	if rl == nil || !rl.ResetAt.Equal(time.Unix(1735689600, 0)) {
		t.Fatalf("rl = %+v", rl)
	}
	rl = detectRateLimit("rate limit; resets 1735689600123")
	if rl == nil || !rl.ResetAt.Equal(time.UnixMilli(1735689600123)) {
		t.Fatalf("rl = %+v", rl)
	}
}

// Blank lines in the stream are skipped (not a crash).
func TestStreamBlankLines(t *testing.T) {
	sc := newStreamCapture(context.Background())
	feed(sc, "\n\n   \n"+`{"type":"result","subtype":"success","result":"ok"}`+"\n\n", 3)
	res, err := parseOutput(processOutput{Stdout: sc.Bytes()}, false)
	if err != nil || res.Output != "ok" {
		t.Fatalf("%+v %v", res, err)
	}
}

// A usage-limit message printed as plain text (no result event) is still
// detected: the non-event output reaches parseOutput.
func TestStreamPlainTextRateLimit(t *testing.T) {
	sc := newStreamCapture(context.Background())
	feed(sc, `{"type":"system","subtype":"init"}`+"\nClaude AI usage limit reached|1735689600\n", 5)
	_, err := parseOutput(processOutput{Stdout: sc.Bytes(), ExitCode: 1, Err: errors.New("exit 1")}, true)
	var rl *core.ErrClaudeRateLimited
	if !errors.As(sc.refineError(err), &rl) || !rl.ResetAt.Equal(time.Unix(1735689600, 0)) {
		t.Fatalf("err = %v", err)
	}
}
