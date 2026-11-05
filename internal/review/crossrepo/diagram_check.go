package crossrepo

// Syntax check of the summary diagram with the real Mermaid parser.
// sanitizeDiagram (diagram.go) only checks security and size; whether GitLab
// can render the diagram is decided by Mermaid itself. The parser runs in the
// sandbox image (tools/mermaid-check, Dockerfile.sandbox) as an exec
// container: --network none, no credentials, the diagram on stdin only.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

const (
	// diagramCheckTimeout bounds one parser run (container start included).
	diagramCheckTimeout = 10 * time.Second
	// maxDiagramCheckBytes caps the stdin of the parser (a diagram within
	// the line caps of sanitizeDiagram is far below it).
	maxDiagramCheckBytes = 64 << 10
	// maxDiagramMessage caps the parser message kept for the model and the log.
	maxDiagramMessage = 600
)

// mermaidCheckCmd is the checker script of the sandbox image.
var mermaidCheckCmd = []string{"node", "/opt/mermaid-check/check.mjs"}

// DiagramChecker parses a Mermaid diagram. msg is the parser's complaint
// ("" = the diagram parses); err means the check itself could not run (no
// parser in the image, timeout, docker failure): callers then keep the
// diagram, as it was before the check.
type DiagramChecker interface {
	Check(ctx context.Context, diagram string) (msg string, err error)
}

// ExecDiagramChecker runs the parser in a sandbox exec container.
type ExecDiagramChecker struct {
	Runner core.Runner
}

// Check implements DiagramChecker. The script exits 0 when the diagram
// parses, 1 with the parser message on stdout on a syntax error and 2 when
// the check itself failed.
func (c ExecDiagramChecker) Check(ctx context.Context, diagram string) (string, error) {
	if c.Runner == nil {
		return "", errors.New("no runner")
	}
	if len(diagram) > maxDiagramCheckBytes {
		return "", fmt.Errorf("diagram too large to check (%d bytes)", len(diagram))
	}
	dir, err := os.MkdirTemp("", "reviewbot-mermaid-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	res, err := c.Runner.Exec(ctx, core.ExecRequest{Dir: dir, Command: mermaidCheckCmd, Stdin: diagram, Timeout: diagramCheckTimeout})
	if err != nil {
		return "", err
	}
	switch res.ExitCode {
	case 0:
		return "", nil
	case 1:
		msg := mdtext.TruncateKeepLines(strings.TrimSpace(res.Stdout), maxDiagramMessage)
		if msg == "" {
			msg = "syntax error"
		}
		return msg, nil
	default:
		return "", fmt.Errorf("mermaid check exit %d: %s", res.ExitCode, mdtext.Shorten(strings.TrimSpace(res.Stderr), 200))
	}
}

// CheckDiagram is what validate_diagram answers and the final gate decides:
// problem != "" is why the diagram is not fit to render (sanitizeDiagram's
// rule or the parser's message); err is a check that could not run.
func CheckDiagram(ctx context.Context, chk DiagramChecker, diagram string) (problem string, err error) {
	d, serr := sanitizeDiagram(diagram)
	if serr != nil {
		return serr.Error(), nil
	}
	if d == "" || chk == nil {
		return "", nil
	}
	return chk.Check(ctx, d)
}

// DiagramChecker is the parser of this deployment's sandbox image.
func (xr *Feature) DiagramChecker() DiagramChecker {
	return ExecDiagramChecker{Runner: xr.env.Runner}
}
