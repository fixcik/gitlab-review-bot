package claude

import (
	"context"
	"errors"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// isAdvisorError reports whether a failed run was refused because of the
// advisor tool (Claude Code 2.1.x messages: "does not support the advisor
// tool", "is not a valid advisor model", "The configured advisor model is
// not compatible…", "the advisor tool is not available", "cannot advise").
func isAdvisorError(err error) bool {
	var rl *core.ErrClaudeRateLimited
	if err == nil || errors.As(err, &rl) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range advisorErrors {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// advisorErrors are the CLI's advisor refusals (lower case). A bare
// "advisor" is not enough: the error text of a failed run can include the
// model's own prose, which would switch the advisor off for the process.
var advisorErrors = []string{
	"does not support the advisor",
	"not a valid advisor",
	"advisor model is not compatible",
	"advisor model is not supported",
	"advisor tool is not available",
	"advisor tool is not enabled",
	"advisor is not available",
	"cannot advise",
	"unknown option '--advisor'",
	"unknown option: --advisor",
	"unknown option --advisor",
}
