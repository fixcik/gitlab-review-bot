package reviewtest

import (
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
)

// EnableMCP turns review.mcp on for the harness (docker mode: the client URL
// is derived from sandbox.egress_proxy).
// A harness that has a server already keeps it.
func EnableMCP(h *Harness) *mcpserver.Server {
	if h.Deps.MCP == nil {
		h.Deps.MCP = mcpserver.New(mcpserver.Options{})
	}
	h.Cfg.Review.MCP.Enabled = true
	return h.Deps.MCP
}

func LintResult(fs ...lint.Finding) *lint.Result {
	var tools []string
	seen := map[string]bool{}
	for _, f := range fs {
		if !seen[f.Tool] {
			seen[f.Tool] = true
			tools = append(tools, f.Tool)
		}
	}
	return &lint.Result{Findings: fs, Tools: Ran(tools...)}
}
