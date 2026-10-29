package review_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

// routeHarness: an MR that changes README.md only (3 lines).
func routeHarness(t *testing.T) (*reviewtest.Harness, *[]string) {
	t.Helper()
	h := reviewtest.New(t)
	h.Repos.Diff, h.Repos.Files = review.DocsDiff, []string{"README.md"}
	h.Idx.Lines["README.md"] = map[int]int{1: 1, 2: 0, 3: 0}
	var routes []string
	h.Deps.OnRoute = func(r string) { routes = append(routes, r) }
	return h, &routes
}

func TestLightRouteDocsOnly(t *testing.T) {
	h, routes := routeHarness(t)
	reviewtest.EnableMCP(h)                               // the light route runs without MCP
	h.Cfg.Review.Verify = config.Defaults().Review.Verify // verification on: nothing to verify on the light route
	minor := output.Finding{Path: "README.md", Line: 2, Severity: "minor", Title: "Опечатка", Body: "абзац"}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{minor}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("runs %d, want 1 (no verification)", len(h.Runner.Reqs))
	}
	req := h.Runner.Reqs[0]
	if req.MCP != nil || strings.Contains(req.SystemPrompt, "submit_finding") {
		t.Fatalf("light run got MCP: %+v", req.MCP)
	}
	if req.Model != "haiku" || req.Advisor != "" || req.SearchModel != "haiku" || req.DeepReviewModel != "" ||
		strings.Contains(req.SystemPrompt, "advisor tool") || strings.Contains(req.SystemPrompt, "deep-review") {
		t.Fatalf("light run: model %q advisor %q search %q", req.Model, req.Advisor, req.SearchModel)
	}
	if len(*routes) != 1 || (*routes)[0] != review.RouteLight {
		t.Fatalf("routes %v", *routes)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "модель: haiku (низкий риск: только docs, 3 строки)") {
		t.Fatalf("summary footer:\n%s", sum)
	}
}

func TestLightRouteCriticalStillVerifiedWithNormalModel(t *testing.T) {
	h, _ := routeHarness(t)
	h.Cfg.Review.Verify = config.Defaults().Review.Verify
	crit := output.Finding{Path: "README.md", Line: 2, Severity: "critical", Title: "В документации ключ API", Body: "абзац"}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{crit}, nil)}, {Out: reviewtest.VerdictsOut(t, verify.FindingVerdict{ID: "F1", Verdict: verify.VerdictConfirm, Reason: "есть"})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("runs %d, want review + verification", len(h.Runner.Reqs))
	}
	// Verification is not the review: no deep-review, the advisor note as before.
	if v := h.Runner.Reqs[1]; v.Model != "sonnet" || v.Advisor != "opus" || v.DeepReviewModel != "" || strings.Contains(v.SystemPrompt, "deep-review") {
		t.Fatalf("verification on the light route: model %q advisor %q", v.Model, v.Advisor)
	}
}

func TestNormalRouteForCodeAndRoutingOff(t *testing.T) {
	for _, off := range []bool{false, true} {
		h := reviewtest.New(t)
		reviewtest.EnableMCP(h)
		h.Cfg.Review.Routing.Enabled = !off
		var routes []string
		h.Deps.OnRoute = func(r string) { routes = append(routes, r) }
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		req := h.Runner.Reqs[0]
		if req.Model != "sonnet" || req.Advisor != "opus" || req.MCP == nil {
			t.Fatalf("off=%v: model %q advisor %q mcp %v", off, req.Model, req.Advisor, req.MCP)
		}
		if len(routes) != 1 || routes[0] != review.RouteNormal {
			t.Fatalf("off=%v: routes %v", off, routes)
		}
		sum := reviewtest.SummaryBody(t, h)
		if got := strings.Contains(sum, "модель: sonnet"); got == off {
			t.Fatalf("off=%v: footer route shown=%v:\n%s", off, got, sum)
		}
	}
}

// An incremental docs-only range of a big MR goes to the normal model.
func TestRouteBigMRIncremental(t *testing.T) {
	h, routes := routeHarness(t)
	h.Cfg.Review.SkipTrivialIncrements = false
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old0000000000000"}
	h.Repos.Ancestors["old0000000000000"] = true
	h.Cfg.Review.TBD.MaxLines = 2 // the fake full diff (the same 3 lines) is over it
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if h.Runner.Reqs[0].Model != "sonnet" || (*routes)[0] != review.RouteNormal {
		t.Fatalf("model %q routes %v", h.Runner.Reqs[0].Model, *routes)
	}
	if !slices.ContainsFunc(h.Repos.DiffFrom, func(s string) bool { return strings.EqualFold(s, "base000000000000") }) {
		t.Fatalf("whole MR diff not computed: %v", h.Repos.DiffFrom)
	}
}

// A repository cannot change models: an old routing section makes its
// config invalid (no review), it never upgrades the light model.
func TestRouteRepoCannotChangeModel(t *testing.T) {
	h, _ := routeHarness(t)
	h.GL.RepoConfig = "routing:\n  light_model: opus\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	for _, req := range h.Runner.Reqs {
		if req.Model == "opus" {
			t.Fatalf("repo config changed the model: %+v", req)
		}
	}
}

// The heavy route reviews with heavy_model, without the advisor and the
// deep-review subagent (the search subagent and MCP stay), and is counted
// as "heavy".
func TestHeavyRoute(t *testing.T) {
	h := reviewtest.New(t)
	reviewtest.EnableMCP(h)
	h.Cfg.Review.Routing.HeavyModel, h.Cfg.Review.Routing.HeavyMinLines = "opus", 1
	var routes []string
	h.Deps.OnRoute = func(r string) { routes = append(routes, r) }
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if req.Model != "opus" || req.Advisor != "" || req.DeepReviewModel != "" || req.SearchModel != "haiku" || req.MCP == nil {
		t.Fatalf("heavy run: model %q advisor %q deep %q search %q mcp %v", req.Model, req.Advisor, req.DeepReviewModel, req.SearchModel, req.MCP != nil)
	}
	if strings.Contains(req.SystemPrompt, "deep-review") || strings.Contains(req.SystemPrompt, "advisor tool") || !strings.Contains(req.SystemPrompt, "`code-search` subagent") {
		t.Errorf("heavy helpers note:\n%s", req.SystemPrompt)
	}
	if len(routes) != 1 || routes[0] != review.RouteHeavy {
		t.Fatalf("routes %v", routes)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "модель: opus (тяжёлый: ") {
		t.Fatalf("summary footer:\n%s", sum)
	}
}
