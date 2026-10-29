package claude

import (
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Token usage in the `claude -p --output-format json` result object.
// Field names verified against Claude Code 2.1.310 (result schema in the CLI;
// code.claude.com/docs/en/agent-sdk/cost-tracking):
//
//	"usage": {"input_tokens", "output_tokens", "cache_creation_input_tokens",
//	          "cache_read_input_tokens", ...}            // main loop only
//	"modelUsage": {"<model>": {"inputTokens", "outputTokens",
//	          "cacheReadInputTokens", "cacheCreationInputTokens",
//	          "costUSD", ...}}                           // subagents included
//
// Missing fields decode as zero (the cache counters may be null).
type cliUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type cliModelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

// usage returns the run's token totals and per-model breakdown. The totals
// are the sum of modelUsage when present (it also counts subagents, which
// the top-level usage leaves out), else the top-level usage.
func (r *cliResult) usage() (core.TokenUsage, map[string]core.ModelUsage) {
	top := core.TokenUsage{}
	if r.Usage != nil {
		top = core.TokenUsage{
			Input: r.Usage.InputTokens, Output: r.Usage.OutputTokens,
			CacheRead: r.Usage.CacheReadInputTokens, CacheWrite: r.Usage.CacheCreationInputTokens,
		}
	}
	if len(r.ModelUsage) == 0 {
		return top, nil
	}
	models := make(map[string]core.ModelUsage, len(r.ModelUsage))
	var sum core.TokenUsage
	for name, m := range r.ModelUsage {
		mu := core.ModelUsage{
			TokenUsage: core.TokenUsage{
				Input: m.InputTokens, Output: m.OutputTokens,
				CacheRead: m.CacheReadInputTokens, CacheWrite: m.CacheCreationInputTokens,
			},
			CostUSD: m.CostUSD,
		}
		models[name] = mu
		sum = sum.Add(mu.TokenUsage)
	}
	if sum.Total() == 0 {
		return top, models
	}
	return sum, models
}

// metrics fills the usage fields of a RunResult from the CLI result.
func (r *cliResult) metrics() core.RunResult {
	u, m := r.usage()
	return core.RunResult{
		CostUSD:  r.TotalCostUSD,
		Duration: time.Duration(r.DurationMS) * time.Millisecond,
		NumTurns: r.NumTurns,
		Usage:    u,
		Models:   m,
	}
}

// withUsage wraps err in *core.FailedRun when the failed run reported a
// cost or tokens, so the usage meter can still account for it.
func withUsage(r *cliResult, err error) error {
	p := r.metrics()
	if p.CostUSD == 0 && p.Usage.Total() == 0 {
		return err
	}
	return &core.FailedRun{Err: err, Partial: p}
}
