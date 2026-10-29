package review_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/claude"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// requestText renders a run request like the golden files hold it.
func requestText(req core.RunRequest) string {
	base := "none"
	if req.BaseDir != "" {
		base = "mounted"
	}
	return "=== SYSTEM ===\n" + req.SystemPrompt + "\n=== PROMPT ===\n" + req.Prompt +
		"\n=== ALLOWED ===\n" + strings.Join(claude.AllowedTools(req), "\n") + "\n=== BASE ===\n" + base + "\n"
}

// The CLI reports the bot's server as failed: the answer is discarded and
// the review runs once more with exactly the request of MCP off (the full
// prompt sections, the "problems" array, no MCP tools); the extra run is
// checked against the global limit and recorded; the next review skips MCP
// (circuit breaker) and runs once.
func TestMCPFallbackRerun(t *testing.T) {
	h, _ := reviewtest.RichHarness(t)
	srv := reviewtest.EnableMCP(h)
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOutCompat(t, nil, nil), MCPStatus: "failed"},
		{Out: reviewtest.ReviewOutCompat(t, nil, nil)},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("runs: %d", len(h.Runner.Reqs))
	}
	if h.Runner.Reqs[0].MCP == nil || h.Runner.Reqs[1].MCP != nil {
		t.Fatalf("MCP: first %v, re-run %v", h.Runner.Reqs[0].MCP, h.Runner.Reqs[1].MCP)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "prompt_mcp_off.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got := requestText(h.Runner.Reqs[1]); got != string(want) {
		t.Errorf("the re-run is not the MCP-off request:\n%s", got)
	}
	if h.Lim.GlobalCalls < 1 || len(h.Lim.Recs) != 2 || len(h.Runner.RecAt) != 2 || h.Runner.RecAt[1] != 2 {
		t.Errorf("limits: global checks %d, records %d, records at runs %v", h.Lim.GlobalCalls, len(h.Lim.Recs), h.Runner.RecAt)
	}
	if srv.Sessions() != 0 {
		t.Errorf("sessions left: %d", srv.Sessions())
	}
	if on, _ := srv.Degraded(); !on {
		t.Fatal("circuit breaker not open")
	}
	// The next review goes without MCP at once: one run, no fallback.
	reviewtest.NewHead(h, "head222222222222")
	h.Runner.Reqs, h.Runner.RecAt = nil, nil
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || h.Runner.Reqs[0].MCP != nil {
		t.Fatalf("after the fallback: %d runs, MCP %v", len(h.Runner.Reqs), h.Runner.Reqs[0].MCP)
	}
}
