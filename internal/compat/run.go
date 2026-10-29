package compat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// timeout is the limit of one tool run.
func (c *Checker) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

// run executes argv in dir and classifies the outcome. ok=false means an
// Issue was recorded and the output must not be parsed.
func (c *Checker) run(ctx context.Context, dir string, argv []string, source, subject string, res *Result) (*core.ExecResult, bool) {
	out, err := c.Runner.Exec(ctx, core.ExecRequest{Dir: dir, Command: argv, Timeout: c.timeout()})
	if err != nil {
		reason := "не удалось запустить: " + shortText(err.Error(), 300)
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "превышено время выполнения"
		}
		res.Issues = append(res.Issues, Issue{Source: source, Path: subject, Kind: IssueFailed, Reason: reason})
		return nil, false
	}
	if out.ExitCode == 127 || notFoundText(out.Stderr) {
		res.Issues = append(res.Issues, Issue{Source: source, Path: subject, Kind: IssueUnavailable,
			Reason: fmt.Sprintf("%s не установлен в sandbox-образе", argv[0])})
		return nil, false
	}
	return out, true
}

func notFoundText(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "executable file not found") && strings.Contains(s, "$path")
}

func (c *Checker) failed(res *Result, source, subject, why string, out *core.ExecResult) {
	reason := why
	if out != nil {
		detail := strings.TrimSpace(out.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(out.Stdout)
		}
		if detail != "" {
			reason += ": " + shortText(stripANSI(detail), 300)
		}
	}
	res.Issues = append(res.Issues, Issue{Source: source, Path: subject, Kind: IssueFailed, Reason: reason})
}
func shortText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
