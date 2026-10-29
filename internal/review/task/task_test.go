package task

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/tasklink"
)

func TestReplaceTitleHintAndInlineCode(t *testing.T) {
	block := renderTitleHint(tasklink.Hint{Messages: []string{"m"}, SuggestedTitle: "feat: a `b` (X-1)"}, &tasklink.Suggestion{Type: "feat", Description: "a --> b"})
	if strings.Contains(block, "a --> b") {
		t.Fatalf("suggestion JSON must not close the HTML comment: %s", block)
	}
	if s := ParseTitleSuggestion(block); s == nil || s.Description != "a --> b" {
		t.Fatalf("roundtrip: %+v", s)
	}
	if !strings.Contains(block, "``feat: a `b` (X-1)``") {
		t.Fatalf("inline code: %s", block)
	}
	body := "x\n" + marker.TitleHintStart + "\nold\n" + marker.TitleHintEnd + "\ny"
	if got := replaceTitleHint(body, "NEW"); got != "x\nNEW\ny" {
		t.Fatalf("got %q", got)
	}
}
