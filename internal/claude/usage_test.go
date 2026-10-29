package claude

import (
	"errors"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// A result shaped like Claude Code 2.1.x `--output-format json` output.
const resultWithUsage = `{"type":"result","subtype":"success","is_error":false,"duration_ms":12000,"num_turns":4,
"result":"{\"ok\":true}","total_cost_usd":0.75,
"usage":{"input_tokens":10,"output_tokens":200,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,
  "server_tool_use":{"web_search_requests":0},"service_tier":"standard"},
"modelUsage":{
  "claude-opus-4-7":{"inputTokens":10,"outputTokens":200,"cacheReadInputTokens":4000,"cacheCreationInputTokens":300,"webSearchRequests":0,"costUSD":0.7,"contextWindow":200000},
  "claude-haiku-4-5":{"inputTokens":5,"outputTokens":7,"cacheReadInputTokens":0,"cacheCreationInputTokens":1,"webSearchRequests":0,"costUSD":0.05,"contextWindow":200000}}}`

func TestParseOutputUsage(t *testing.T) {
	res, err := parseOutput(processOutput{Stdout: []byte(resultWithUsage)}, true)
	if err != nil {
		t.Fatal(err)
	}
	// Totals come from modelUsage (includes subagents).
	want := core.TokenUsage{Input: 15, Output: 207, CacheRead: 4000, CacheWrite: 301}
	if res.Usage != want {
		t.Fatalf("usage = %+v, want %+v", res.Usage, want)
	}
	if res.CostUSD != 0.75 || res.NumTurns != 4 || res.Duration != 12*time.Second {
		t.Fatalf("metrics: %+v", res)
	}
	if len(res.Models) != 2 {
		t.Fatalf("models: %+v", res.Models)
	}
	o := res.Models["claude-opus-4-7"]
	if o.Input != 10 || o.Output != 200 || o.CacheRead != 4000 || o.CacheWrite != 300 || o.CostUSD != 0.7 {
		t.Fatalf("opus: %+v", o)
	}
	if res.Models["claude-haiku-4-5"].CostUSD != 0.05 {
		t.Fatalf("haiku: %+v", res.Models["claude-haiku-4-5"])
	}
}

func TestParseOutputUsageWithoutModelUsage(t *testing.T) {
	out := `{"type":"result","subtype":"success","result":"hi","total_cost_usd":0.1,
"usage":{"input_tokens":3,"output_tokens":4,"cache_creation_input_tokens":null,"cache_read_input_tokens":5}}`
	res, err := parseOutput(processOutput{Stdout: []byte(out)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage != (core.TokenUsage{Input: 3, Output: 4, CacheRead: 5}) || res.Models != nil {
		t.Fatalf("got %+v / %+v", res.Usage, res.Models)
	}
}

func TestParseOutputNoUsageFields(t *testing.T) {
	res, err := parseOutput(processOutput{Stdout: []byte(`{"type":"result","subtype":"success","result":"hi"}`)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage != (core.TokenUsage{}) || res.Models != nil || res.CostUSD != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestParseOutputFailedRunKeepsUsage(t *testing.T) {
	out := `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":30,"total_cost_usd":1.5,
"usage":{"input_tokens":100,"output_tokens":50}}`
	_, err := parseOutput(processOutput{Stdout: []byte(out), ExitCode: 1}, true)
	var fr *core.FailedRun
	if !errors.As(err, &fr) {
		t.Fatalf("want *core.FailedRun, got %T %v", err, err)
	}
	if fr.Partial.CostUSD != 1.5 || fr.Partial.Usage.Input != 100 || fr.Partial.NumTurns != 30 {
		t.Fatalf("partial: %+v", fr.Partial)
	}

	// Rate limits stay detectable through the wrapper.
	rl := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Claude AI usage limit reached|1735689600","total_cost_usd":0.2}`
	_, err = parseOutput(processOutput{Stdout: []byte(rl)}, true)
	var e *core.ErrClaudeRateLimited
	if !errors.As(err, &e) || !errors.As(err, &fr) {
		t.Fatalf("want rate limit inside FailedRun, got %T %v", err, err)
	}

	// Nothing reported -> plain error, no wrapper.
	_, err = parseOutput(processOutput{Stdout: []byte(`{"type":"result","subtype":"error_max_turns","is_error":true}`)}, true)
	if errors.As(err, &fr) {
		t.Fatalf("unexpected FailedRun: %v", err)
	}
}
