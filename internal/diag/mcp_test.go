package diag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// The per-run MCP token reaches the CLI only through the MCP config file:
// nothing of it may land in the run record (argv, request, transcript).
func TestMCPTokenNotRecorded(t *testing.T) {
	const token = "0123abcd0123abcd0123abcd0123abcd0123abcd0123abcd0123abcd0123abcd"
	e := newEnv(t)
	work := t.TempDir()
	runner := e.rec.WrapRunner(newClaudeRunner(t, fakeDocker(t, "")))
	var id string
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
		id = FromContext(ctx).ID()
		_, err := runner.Run(ctx, core.RunRequest{Dir: work, Prompt: "review", JSONSchema: `{"type":"object"}`,
			MCP: &core.MCPServer{Name: "reviewbot", URL: "http://egress:8091/mcp", Token: token, Tools: []string{"submit_finding"}}})
		return err
	})
	if err := h(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	_, r := e.record(t, id)
	if len(r.Claude) != 1 {
		t.Fatalf("claude runs %d", len(r.Claude))
	}
	c := r.Claude[0]
	if c.MCPURL != "http://egress:8091/mcp" || strings.Join(c.MCPTools, ",") != "submit_finding" {
		t.Errorf("mcp info %q %v", c.MCPURL, c.MCPTools)
	}
	argv := strings.Join(c.Argv, " ")
	if !strings.Contains(argv, "--mcp-config <mcp config:") || !strings.Contains(argv, "mcp__reviewbot__submit_finding") {
		t.Errorf("argv %s", argv)
	}
	err := filepath.Walk(e.rec.Root(), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), token) {
			t.Errorf("MCP token persisted in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
