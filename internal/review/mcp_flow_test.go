package review_test

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestMCPPromptSwitch(t *testing.T) {
	h, _ := reviewtest.RichHarness(t)
	srv := reviewtest.EnableMCP(h)
	got := renderedRequest(t, h)
	checkGolden(t, "prompt_mcp_on.golden", got)
	req := h.Runner.Reqs[0]
	if req.MCP == nil || req.MCP.URL != "http://egress:8091/mcp" || req.MCP.Name != "reviewbot" || len(req.MCP.Token) != 64 {
		t.Fatalf("RunRequest.MCP = %v", req.MCP)
	}
	tools := strings.Join(req.MCP.Tools, ",")
	if tools != "diff_lines,api_changes,lint_findings,coverage,map_lookup,migrations,submit_finding,withdraw_finding,list_findings,report_progress,report_problem" {
		t.Errorf("tools %s", tools)
	}
	for _, s := range []string{
		"## Review bot tools (MCP server \"reviewbot\")", "submit EVERY finding with `submit_finding`",
		"\"findings\" may be an empty array", "`api_changes`: the full API changelog",
	} {
		if !strings.Contains(req.SystemPrompt, s) {
			t.Errorf("system prompt lacks %q", s)
		}
	}
	// The per-MR summaries (coverage, system map) are in the user prompt, not
	// the system prompt (prompt caching).
	for _, s := range []string{"Call `api_changes`", "Call `lint_findings`", "Call `migrations`", "## Backward compatibility", "Call `map_lookup`", "Call `coverage`"} {
		if !strings.Contains(req.Prompt, s) {
			t.Errorf("prompt lacks %q", s)
		}
	}
	// The big sections moved behind the tools.
	for _, s := range []string{"<untrusted_api_changes>", "<untrusted_migration_facts>", "Detected non-static command", "Added endpoints:"} {
		if strings.Contains(req.Prompt, s) {
			t.Errorf("prompt still carries %q", s)
		}
	}
	if strings.Contains(req.SystemPrompt+req.Prompt, "src/api/orders.ts") || strings.Contains(req.SystemPrompt+req.Prompt, "Least covered") ||
		strings.Contains(req.SystemPrompt, "Call `map_lookup`") || strings.Contains(req.SystemPrompt, "Call `coverage`") {
		t.Error("the prompts still list map usages / coverage details, or the system prompt has per-MR summaries")
	}
	if strings.Contains(got, req.MCP.Token) {
		t.Error("token in the prompt")
	}
	// The run is over: the session is gone.
	if srv.Sessions() != 0 {
		t.Errorf("sessions left after the review: %d", srv.Sessions())
	}
}
