package review_test

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/claude"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/*.golden")

// renderedRequest is what the golden file holds: system prompt, prompt,
// allowed tools (as the runner passes them to the CLI) and whether the merge
// base is mounted (/base) for the review run.
func renderedRequest(t *testing.T, h *reviewtest.Harness) string {
	t.Helper()
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) == 0 {
		t.Fatal("no claude run")
	}
	req := h.Runner.Reqs[0]
	base := "none"
	if req.BaseDir != "" {
		base = "mounted"
	}
	return "=== SYSTEM ===\n" + req.SystemPrompt + "\n=== PROMPT ===\n" + req.Prompt +
		"\n=== ALLOWED ===\n" + strings.Join(claude.AllowedTools(req), "\n") + "\n=== BASE ===\n" + base + "\n"
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update-golden to create): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the rendered request (MCP off must keep the prompt byte for byte):\n--- got ---\n%s", name, got)
	}
}

// TestPromptWithoutMCPUnchanged pins the review request as it was before the
// MCP server existed: with review.mcp off, nothing may change. Deliberate
// changes since: the "Problems of the review itself" paragraph of
// review.problems (on by default; TestProblemsOffKeepsRequest checks that
// turning it off restores the request), and the read-only model (no Bash:
// the tools rule, /base instead of git, the coverage lines in the prompt
// instead of the diffcov tool, no "Allowed shell commands" list).
//
// review.mcp is on by default (prompt_mcp_on.golden is the default path);
// this request is what MCP off gives: the service switch off, no server,
// the repository's mcp.enabled: false, or the fallback re-run
// (mcp_fallback_test.go).
func TestPromptWithoutMCPUnchanged(t *testing.T) {
	h, _ := reviewtest.RichHarness(t)
	h.Cfg.Review.MCP.Enabled = false
	checkGolden(t, "prompt_mcp_off.golden", renderedRequest(t, h))
}

// A repository that turns MCP off gets exactly the MCP-off request, with
// the server running.
func TestPromptRepoMCPOff(t *testing.T) {
	h, _ := reviewtest.RichHarness(t)
	srv := reviewtest.EnableMCP(h)
	h.GL.RepoConfig += "\nmcp:\n  enabled: false\n"
	got := renderedRequest(t, h)
	want, err := os.ReadFile(filepath.Join("testdata", "prompt_mcp_off.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("repo mcp.enabled: false differs from prompt_mcp_off.golden:\n%s", got)
	}
	if h.Runner.Reqs[0].MCP != nil || srv.Sessions() != 0 {
		t.Error("MCP session opened for a repository that turned it off")
	}
	if on, _ := srv.Degraded(); on {
		t.Error("a repository opt-out opened the breaker")
	}
}
