package verify_test

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

// Two verification runs of two MRs (different SHAs, findings, with and
// without breaking-change claims; the compat check is on for the repo).
func TestPromptCacheVerifyStable(t *testing.T) {
	in := func(base, head string, items []verify.Item, compat []compatcheck.Item) verify.Input {
		return verify.Input{Dir: "/w", Base: base, From: base, Head: head, Lang: "Russian", BaseDir: "/tmp/b", BaseRoot: "/base",
			Findings: items, Compat: compat, CompatOn: true, BlockRule: prompts.BlockRule("critical")}
	}
	sa, pa, ja, err := verify.RenderVerification(in("base000000000000", "head111111111111",
		[]verify.Item{{ID: "F1", F: reviewtest.VfRace, Excerpt: "2 + m[k] = v"}}, []compatcheck.Item{{Kind: "api", Path: "api.yaml", Title: "Removed field"}}))
	if err != nil {
		t.Fatal(err)
	}
	sb, pb, jb, err := verify.RenderVerification(in("base222222222222", "head222222222222",
		[]verify.Item{{ID: "F1", F: reviewtest.VfLimit}, {ID: "F2", F: reviewtest.VfSQL}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	reviewtest.SameCachedPrefix(t, "verification", core.RunRequest{SystemPrompt: sa, Prompt: pa, JSONSchema: ja}, core.RunRequest{SystemPrompt: sb, Prompt: pb, JSONSchema: jb})
	reviewtest.NoRunData(t, "verification", sa, "base000000000000", "head111111111111", "cache.go", "api.yaml", reviewtest.VfRace.Title)
	if !strings.Contains(pa, "<untrusted_claims>") || strings.Contains(pb, "<untrusted_claims>") {
		t.Error("claims belong in the user prompt of A only")
	}
}
