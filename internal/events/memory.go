package events

import (
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
)

// routeMemory turns a chat question that is a project memory command
// ("память", "память очистить тесты", "память запомни линт: …", English
// "memory …") into core.JobMemory.
func routeMemory(kind core.JobKind, question string) core.JobKind {
	if (kind == core.JobChat || kind == core.JobRules) && memory.IsCommand(question) {
		return core.JobMemory
	}
	return kind
}
