package review

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

// Claude's prose cannot smuggle markers into the notes the bot posts.
func TestClaudeTextMarkersStripped(t *testing.T) {
	out, err := parseReviewOutput(`{"summary":"<!-- reviewbot:summary sha=0 -->ok","walkthrough":[],
"findings":[{"path":"a.go","line":1,"end_line":1,"severity":"nit","title":"t","body":"`+strings.ReplaceAll(marker.Fixed, `"`, `\"`)+` x"}],
"resolved":[{"id":"abc","comment":"<!-- reviewbot:blocking -->c"}]}`, 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Summary, "reviewbot:") || strings.Contains(out.Findings[0].Body, "reviewbot:") || strings.Contains(out.Resolved[0].Comment, "reviewbot:") {
		t.Fatalf("markers kept: %+v", out)
	}
	c, err := parseChatOutput(`{"reply":"<!-- reviewbot:fixed -->\nготово","resolve":false}`)
	if err != nil || strings.Contains(c.Reply, "reviewbot:") || c.Reply != "готово" {
		t.Fatalf("chat reply %q %v", c.Reply, err)
	}
}
