package reviewtest

import (
	"strconv"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/claude"
	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Prompt caching (prompt_cache.go, docs/DESIGN.md "Prompt layout and
// caching"): two runs of the same kind in the same repository must send
// byte-identical system prompts, answer schemas and tool lists, whatever the
// merge request; only the user prompt differs.

// SameCachedPrefix asserts the parts of two requests that form the cached
// prefix (tools incl. the --json-schema tool, system prompt) are identical.
func SameCachedPrefix(t *testing.T, kind string, a, b core.RunRequest) {
	t.Helper()
	if a.SystemPrompt != b.SystemPrompt {
		t.Errorf("%s: system prompts differ:\n%s", kind, FirstDiff(a.SystemPrompt, b.SystemPrompt))
	}
	if a.JSONSchema != b.JSONSchema {
		t.Errorf("%s: schemas differ:\n%s", kind, FirstDiff(a.JSONSchema, b.JSONSchema))
	}
	if x, y := strings.Join(claude.AllowedTools(a), "\n"), strings.Join(claude.AllowedTools(b), "\n"); x != y {
		t.Errorf("%s: allowed tools differ:\n%s\n---\n%s", kind, x, y)
	}
	if a.Model != b.Model || a.Advisor != b.Advisor || a.SearchModel != b.SearchModel || a.DeepReviewModel != b.DeepReviewModel || a.NoTools != b.NoTools {
		t.Errorf("%s: models / tool set differ: %q %q %q %q %v vs %q %q %q %q %v", kind, a.Model, a.Advisor, a.SearchModel, a.DeepReviewModel, a.NoTools,
			b.Model, b.Advisor, b.SearchModel, b.DeepReviewModel, b.NoTools)
	}
	if a.Prompt == b.Prompt {
		t.Errorf("%s: the user prompts must differ (different MRs)", kind)
	}
}

// FirstDiff shows where two strings start to differ.
func FirstDiff(a, b string) string {
	n := CommonPrefixLen(a, b)
	cut := func(s string) string {
		s = s[n:]
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		return s
	}
	return "at byte " + strconv.Itoa(n) + ":\n<<< " + cut(a) + "\n>>> " + cut(b)
}

func CommonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// NoRunData asserts no SHA, file or MR text of the runs leaked into a
// system prompt.
func NoRunData(t *testing.T, kind, sys string, data ...string) {
	t.Helper()
	for _, d := range data {
		if strings.Contains(sys, d) {
			t.Errorf("%s: system prompt contains run data %q", kind, d)
		}
	}
}
