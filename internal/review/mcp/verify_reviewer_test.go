package mcp_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

// With MCP on, the verifier gets its own read-only session (no
// submit_finding), and findings submitted through MCP in the review are
// verified like the others.
func TestVerifyMCP(t *testing.T) {
	h, _ := reviewtest.VerifyHarness(t)
	srv := reviewtest.EnableMCP(h)
	ts := httptestServer(t, srv)
	h.Cfg.Review.MCP.URL = ts
	run := 0
	h.Runner.OnRun = func(req core.RunRequest) {
		run++
		if req.MCP == nil {
			t.Fatalf("run %d without MCP", run)
		}
		c := &rpcClient{t: t, url: req.MCP.URL, token: req.MCP.Token}
		c.do("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code"}})
		switch run {
		case 1:
			text, _ := c.tool("submit_finding", map[string]any{"path": "cache.go", "line": 2, "severity": "critical", "title": reviewtest.VfRace.Title, "body": reviewtest.VfRace.Body})
			if v := verdictOf(t, text); v.Status != "accepted" {
				t.Errorf("submit: %+v", v)
			}
		case 2:
			if strings.Join(req.MCP.Tools, ",") != "diff_lines" {
				t.Errorf("verifier tools: %v", req.MCP.Tools)
			}
			res, _ := c.do("tools/list", map[string]any{})
			if strings.Contains(string(res), "submit_finding") || !strings.Contains(string(res), "diff_lines") {
				t.Errorf("tools/list: %s", res)
			}
			if text, isErr := c.tool("diff_lines", map[string]any{"path": "cache.go"}); isErr || !strings.Contains(text, "cache.go") {
				t.Errorf("diff_lines: %s", text)
			}
			if !strings.Contains(req.SystemPrompt, "read-only tools of the review bot") {
				t.Error("system prompt lacks the tools note")
			}
		}
	}
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOut(t, nil, nil)},
		{Out: reviewtest.VerdictsOut(t, verify.FindingVerdict{ID: "F1", Verdict: "reject", Reason: "под блокировкой в cache.go:12"})},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if run != 2 || !strings.Contains(h.Runner.Reqs[1].Prompt, reviewtest.VfRace.Title) {
		t.Fatalf("the submitted finding must be verified: runs %d", run)
	}
	if len(reviewtest.FindingThreads(h)) != 0 {
		t.Errorf("rejected: %+v", reviewtest.FindingThreads(h))
	}
	if srv.Sessions() != 0 {
		t.Errorf("sessions left: %d", srv.Sessions())
	}
}

// httptestServer serves the MCP server and returns its endpoint URL.
func httptestServer(t *testing.T, srv *mcpserver.Server) string {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL + mcpserver.Path
}
