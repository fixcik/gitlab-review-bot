package reviewtest

import (
	"encoding/json"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func MustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ReviewOut(t *testing.T, findings []output.Finding, resolved []output.ResolvedFinding) string {
	return MustJSON(t, output.ReviewOutput{
		Summary:     "Добавлен LRU-кэш.",
		Walkthrough: []output.FileSummary{{Path: "cache.go", Summary: "новый кэш | с блокировкой"}},
		Findings:    findings,
		Resolved:    resolved,
	})
}

func LastReply(t *testing.T, g *FakeGitLab) Reply {
	t.Helper()
	if len(g.Replies) == 0 {
		t.Fatalf("no replies")
	}
	return g.Replies[len(g.Replies)-1]
}
