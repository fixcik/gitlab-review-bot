package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/review/mcp"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestMCPOffByRepoOrService(t *testing.T) {
	for name, edit := range map[string]func(h *reviewtest.Harness){
		"service off": func(h *reviewtest.Harness) { h.Cfg.Review.MCP.Enabled = false },
		"repo off":    func(h *reviewtest.Harness) { h.GL.RepoConfig = "mcp:\n  enabled: false\n" },
		"no server":   func(h *reviewtest.Harness) { h.Deps.MCP = nil },
	} {
		h := reviewtest.New(t)
		reviewtest.EnableMCP(h)
		edit(h)
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		req := h.Runner.Reqs[0]
		if req.MCP != nil || strings.Contains(req.SystemPrompt, "Review bot tools") {
			t.Errorf("%s: MCP still on: %v", name, req.MCP)
		}
	}
}

// rpcClient talks JSON-RPC to the MCP endpoint like the CLI does.
type rpcClient struct {
	t     *testing.T
	url   string
	token string
	id    int
}

func (c *rpcClient) do(method string, params any) (json.RawMessage, int) {
	c.t.Helper()
	c.id++
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(b, &out); err != nil || out.Error != nil {
			c.t.Fatalf("%s: %s %v", method, b, err)
		}
	}
	return out.Result, resp.StatusCode
}

// tool calls a tool and returns its text (and isError).
func (c *rpcClient) tool(name string, args any) (string, bool) {
	c.t.Helper()
	res, code := c.do("tools/call", map[string]any{"name": name, "arguments": args})
	if code != http.StatusOK {
		c.t.Fatalf("tools/call %s: HTTP %d", name, code)
	}
	var cr struct {
		Content []struct{ Text string } `json:"content"`
		IsError bool                    `json:"isError"`
	}
	if err := json.Unmarshal(res, &cr); err != nil || len(cr.Content) != 1 {
		c.t.Fatalf("tools/call %s: %s", name, res)
	}
	return cr.Content[0].Text, cr.IsError
}

func verdictOf(t *testing.T, text string) mcp.Verdict {
	t.Helper()
	var v mcp.Verdict
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("verdict %q: %v", text, err)
	}
	return v
}

// The model connects over HTTP, gets a finding rejected, fixes it, and
// answers with an empty findings list: the submitted findings are published
// at the end through the usual pipeline, and the token dies with the run.
func TestMCPEndToEnd(t *testing.T) {
	h, _ := reviewtest.LintHarness(t)
	h.Cfg.Review.Lint.Enabled = true
	h.Deps.Lint.(*reviewtest.FakeLint).Res = reviewtest.LintResult(reviewtest.RuffFinding)
	srv := reviewtest.EnableMCP(h)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	h.Cfg.Review.MCP.URL = ts.URL + mcpserver.Path

	var client *rpcClient
	var steps []string
	h.Runner.OnRun = func(req core.RunRequest) {
		if req.MCP == nil {
			t.Fatal("no MCP in the run request")
		}
		client = &rpcClient{t: t, url: req.MCP.URL, token: req.MCP.Token}
		res, _ := client.do("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code"}})
		if !strings.Contains(string(res), `"protocolVersion":"2025-06-18"`) {
			t.Errorf("initialize: %s", res)
		}
		res, _ = client.do("tools/list", map[string]any{})
		if !strings.Contains(string(res), `"name":"submit_finding"`) || !strings.Contains(string(res), `"name":"lint_findings"`) {
			t.Errorf("tools/list: %s", res)
		}
		// 1. A linter duplicate is rejected with the reason.
		text, _ := client.tool("submit_finding", map[string]any{"path": "cache.go", "line": 3, "severity": "minor",
			"title": "F401: unused import os", "body": "ruff F401: remove it."})
		v := verdictOf(t, text)
		steps = append(steps, v.Status)
		if v.Status != mcp.VerdictRejected || !strings.Contains(strings.Join(v.Reasons, " "), "duplicate of the ci:ruff result F401 at cache.go:3") || v.Hint == "" {
			t.Errorf("lint duplicate: %+v", v)
		}
		// 2. The model fixes it: a deeper problem on the same line.
		text, _ = client.tool("submit_finding", map[string]any{"path": "cache.go", "line": 3, "severity": "major",
			"title": "Кэш не ограничен по размеру", "body": "Нет вытеснения: память растёт."})
		v = verdictOf(t, text)
		steps = append(steps, v.Status+":"+v.ID+":"+v.Placement)
		// 3. Outside the diff: kept for the summary, with the reason.
		text, _ = client.tool("submit_finding", map[string]any{"path": "cache.go", "line": 40, "severity": "minor",
			"title": "Нет документации", "body": "Опишите политику вытеснения."})
		v = verdictOf(t, text)
		steps = append(steps, v.Status+":"+v.ID+":"+v.Placement)
		if list, _ := client.tool("list_findings", nil); !strings.Contains(list, `"id":"F1"`) || !strings.Contains(list, `"id":"F2"`) {
			t.Errorf("list: %s", list)
		}
		client.tool("report_progress", map[string]any{"stage": "writing-answer"})
	}
	// The final answer has no findings: everything was submitted.
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(steps, " ") != "rejected accepted:F1:inline adjusted:F2:summary" {
		t.Errorf("verdicts: %v", steps)
	}
	var inline []string
	for _, d := range h.GL.Published {
		if strings.Contains(d.Body, "reviewbot:finding") {
			inline = append(inline, d.Body)
			if d.Pos == nil || d.Pos.NewLine != 3 {
				t.Errorf("finding position %+v", d.Pos)
			}
		}
	}
	if len(inline) != 1 || !strings.Contains(inline[0], "Кэш не ограничен по размеру") {
		t.Errorf("published findings: %v", inline)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, "Нет документации") || !strings.Contains(sum, "найдено 2") {
		t.Errorf("summary:\n%s", sum)
	}
	// The token is refused after the run.
	if _, code := client.do("ping", nil); code != http.StatusUnauthorized {
		t.Errorf("token after the run: HTTP %d", code)
	}
}
