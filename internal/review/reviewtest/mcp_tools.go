package reviewtest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/review/mcp"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
)

// OpenTestMCP opens an MCP session for the harness MR with the given
// analyzer results (no Claude run).
func OpenTestMCP(t *testing.T, h *Harness, in mcp.Inputs) *mcp.Run {
	t.Helper()
	EnableMCP(h)
	if in.Lang == "" {
		in.Lang = "Russian"
	}
	if in.Threads == nil {
		in.Threads = &mrthread.Threads{Open: map[string]mrthread.OpenFinding{}}
	}
	if in.Checkout == nil {
		co, _ := h.Repos.Checkout(context.Background(), "group/api", &h.GL.MR)
		in.Checkout = co
	}
	m := h.Internals().MCP.Open(context.Background(), AutoJob(), &h.GL.MR, config.Merge(h.Cfg, nil), in)
	if m == nil {
		t.Fatal("no MCP session")
	}
	t.Cleanup(m.Close)
	return m
}

// CallTool calls a tool of the session directly (as tools/call does).
func CallTool(t *testing.T, m *mcp.Run, name string, args any) (string, bool) {
	t.Helper()
	b, _ := json.Marshal(args)
	if args == nil {
		b = nil
	}
	res, known := m.Session().Call(context.Background(), name, b)
	if !known {
		t.Fatalf("tool %s not in the session (tools: %v)", name, m.Server().Tools)
	}
	return res.Text, res.IsError
}
