package mcp_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/mcp"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// The fallback records an mcp_failed problem and counts the reason.
func TestMCPFallbackProblem(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resp    reviewtest.RunResp
		reason  string
		subject string
	}{
		{"failed status", reviewtest.RunResp{MCPStatus: "failed"}, mcp.FallbackNotConnected, "MCP-сервер бота недоступен из песочницы"},
		{"no status, no request", reviewtest.RunResp{MCPStatus: reviewtest.MCPNoStatus}, mcp.FallbackNoRequest, "MCP-сервер бота недоступен из песочницы"},
		{"config dropped", reviewtest.RunResp{MCPDropped: true}, mcp.FallbackDropped, "конфигурация MCP не записана"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ph := reviewtest.NewProbHarness(t, "")
			var reasons []string
			srv := mcpserver.New(mcpserver.Options{OnFallback: func(r string) { reasons = append(reasons, r) }})
			ph.Deps.MCP, ph.Cfg.Review.MCP.Enabled = srv, true
			first := tc.resp
			first.Out = reviewtest.ReviewOut(t, nil, nil)
			if err := ph.Review(t, first, reviewtest.RunResp{Out: reviewtest.ReviewOut(t, nil, nil)}); err != nil {
				t.Fatal(err)
			}
			if len(ph.Runner.Reqs) != 2 || ph.Runner.Reqs[1].MCP != nil {
				t.Fatalf("runs %d", len(ph.Runner.Reqs))
			}
			if got := ph.St.Kinds()[problems.MCPFailed]; len(got) != 1 || got[0] != tc.subject {
				t.Errorf("mcp_failed problems: %v", got)
			}
			if strings.Join(reasons, ",") != tc.reason {
				t.Errorf("reasons %v", reasons)
			}
		})
	}
}

// A connected session without a status ("pending" or none reported) is a
// working MCP run: no fallback.
func TestMCPNoFallbackWhenConnected(t *testing.T) {
	h := reviewtest.New(t)
	srv := reviewtest.EnableMCP(h)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	h.Cfg.Review.MCP.URL = ts.URL + mcpserver.Path
	h.Runner.OnRun = func(req core.RunRequest) {
		if req.MCP != nil {
			c := &rpcClient{t: t, url: req.MCP.URL, token: req.MCP.Token}
			c.do("initialize", map[string]any{"protocolVersion": "2025-06-18"})
		}
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil), MCPStatus: "pending"}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("runs %d", len(h.Runner.Reqs))
	}
	if on, _ := srv.Degraded(); on {
		t.Error("breaker opened for a working run")
	}
}

// The global limit denies the re-run: the answer of the run without the
// tools is used (the model puts its findings into the final answer then),
// nothing more is recorded.
func TestMCPFallbackDeniedUsesFirstAnswer(t *testing.T) {
	h := reviewtest.New(t)
	reviewtest.EnableMCP(h)
	h.Lim.GlobalSeq = []core.Decision{{Allowed: true}, {Allowed: false, Reason: "global limit"}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{{Path: "cache.go", Line: 4, Severity: "major", Title: "Утечка", Body: "x"}}, nil), MCPStatus: "failed"}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(h.Lim.Recs) != 1 {
		t.Fatalf("runs %d, records %d", len(h.Runner.Reqs), len(h.Lim.Recs))
	}
	if !strings.Contains(reviewtest.SummaryBody(t, h), "найдено 1 (🟠 Major: 1)") {
		t.Errorf("the first answer was not used:\n%s", reviewtest.SummaryBody(t, h))
	}
}

// The breaker closes after its cooldown: MCP is used again.
func TestMCPBreakerCloses(t *testing.T) {
	h := reviewtest.New(t)
	now := reviewtest.TestNow
	srv := mcpserver.New(mcpserver.Options{Now: func() time.Time { return now }})
	h.Deps.MCP, h.Cfg.Review.MCP.Enabled = srv, true
	srv.Fallback(mcp.FallbackNoRequest)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if h.Runner.Reqs[0].MCP != nil {
		t.Fatal("MCP used while the breaker is open")
	}
	now = now.Add(mcpserver.DefaultFallbackCooldown)
	reviewtest.NewHead(h, "head222222222222")
	h.Runner.Reqs = nil
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if h.Runner.Reqs[0].MCP == nil {
		t.Fatal("MCP still skipped after the cooldown")
	}
}
