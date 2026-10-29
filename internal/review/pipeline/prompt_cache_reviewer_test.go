package pipeline_test

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// Two CI explanations of failed MR pipelines (different pipelines, SHAs,
// jobs, diffs), and two of the main branch.
func TestPromptCacheCIStable(t *testing.T) {
	render2 := func(d pipeline.CIPromptData) (string, string) {
		t.Helper()
		sys, err := prompts.Render(pipeline.CISystemTmpl, d)
		if err != nil {
			t.Fatal(err)
		}
		p, err := prompts.Render(pipeline.CIUserTmpl, d)
		if err != nil {
			t.Fatal(err)
		}
		return sys, p
	}
	for _, main := range []bool{false, true} {
		var targets []pipeline.CICandidate
		what, title := "merge request", "feat: add cache"
		if main {
			targets, what, title = []pipeline.CICandidate{{IID: 3, Title: "x", Author: "bob"}}, "main branch", ""
		}
		a := pipeline.CIPromptData{What: what, SHA: "head111111111111", Title: title, Failures: "--- job: test\nFAIL TestCache", Diff: "+a", Language: "Russian",
			BaseDir: "/base", Pipeline: 77, JobCount: 1, MR: title != "", Main: main, Targets: targets}
		b := a
		b.SHA, b.Failures, b.Diff, b.Pipeline, b.JobCount, b.Cut = "head222222222222", "--- job: lint\nerr\n--- job: build\nerr", "+b", 78, 2, true
		if main {
			b.Targets = []pipeline.CICandidate{{IID: 4, Title: "y", Author: "alice"}, {IID: 5, Title: "z", Author: "eve"}}
		} else {
			b.Title = "fix: other"
		}
		sa, pa := render2(a)
		sb, pb := render2(b)
		reviewtest.SameCachedPrefix(t, "ci "+what, core.RunRequest{SystemPrompt: sa, Prompt: pa, JSONSchema: pipeline.CISchema(main)},
			core.RunRequest{SystemPrompt: sb, Prompt: pb, JSONSchema: pipeline.CISchema(main)})
		reviewtest.NoRunData(t, "ci", sa, "head111111111111", "TestCache", "77")
	}
}
