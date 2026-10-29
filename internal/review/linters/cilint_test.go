package linters

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

func TestCIPendingMarker(t *testing.T) {
	lr := &Run{ci: &ciLintRun{entries: []ciEntry{
		{st: lint.ToolStatus{Tool: "ci:b", State: lint.StatePending}},
		{st: lint.ToolStatus{Tool: "ci:a", State: lint.StatePending}},
		{st: lint.ToolStatus{Tool: "ci:c", State: lint.StateRan}},
	}}}
	body := "x\n\n" + RenderCIBlock(lr) + "\n\ny"
	got := ciPendingIn(body)
	if len(got) != 2 || !got["ci:a"] || !got["ci:b"] {
		t.Errorf("pending %v in:\n%s", got, body)
	}
	if ciPendingIn("no block") != nil {
		t.Error("no block, nothing pending")
	}
	nb := ReplaceCIBlock(body, marker.CILintStart+"\nnew\n"+marker.CILintEnd)
	if !strings.HasPrefix(nb, "x\n\n"+marker.CILintStart+"\nnew") || !strings.HasSuffix(nb, "y") {
		t.Errorf("replace:\n%s", nb)
	}
	if got := ReplaceCIBlock("body", ""); got != "body" {
		t.Errorf("empty block: %q", got)
	}
}
