package reviewtest

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func ReviewOutCompat(t *testing.T, breaking []output.BreakingChange, resolved []output.ResolvedFinding) string {
	return MustJSON(t, output.ReviewOutput{
		Summary:       "Добавлен LRU-кэш.",
		Walkthrough:   []output.FileSummary{{Path: "cache.go", Summary: "кэш"}},
		Compatibility: &output.Compatibility{Breaking: breaking, Resolved: resolved},
	})
}

var OasdiffChange = compat.Change{
	Source: compat.SourceOasdiff, Path: "cache.go", Line: 3, Rule: "response-property-removed",
	Message: "GET /users/{id}: removed the optional property 'email' from the response", Fingerprint: "fp1",
}

func CompatThreads(h *Harness) []DraftNote {
	var out []DraftNote
	for _, d := range h.GL.Published {
		if _, _, ok := marker.ParseCompat(d.Body); ok {
			out = append(out, d)
		}
	}
	return out
}
