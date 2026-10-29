package review

// Prompt layout for prompt caching.
//
// Claude Code caches the request prefix: tool definitions (built-in tools,
// the StructuredOutput tool made from --json-schema, MCP tools), then the
// system prompt (Claude Code's own + our --append-system-prompt), then the
// messages. A byte that differs invalidates the cache for everything after
// it, and cache hits happen only at block boundaries Claude Code marks (the
// end of the system prompt, the last message), not inside our one-block user
// prompt. So, per kind of run:
//
//   - the system prompt holds only text that is fixed for the bot version
//     and the repository's configuration (rules, block threshold, language,
//     compat/diagram/labels/memory/skills/TBD notes), in a fixed order, with
//     no SHA, count, timestamp or file list;
//   - everything about the merge request or the run (SHAs, diff, coverage,
//     system map facts, migration hints, task, open findings) is in the user
//     prompt, after the static instructions;
//   - a rule that applies only when the user prompt has some section is
//     worded conditionally ("When the prompt contains …") and included for
//     the whole repository, unless that would change the answer schema; then
//     it stays conditional and goes last (the migrations policy).
//
// See docs/DESIGN.md "Prompt layout and caching" and prompt_cache_test.go.

import (
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// factsBlock collects per-MR sections for the end of the user prompt. Each
// section is normalized to "\n<text>\n" (the user template's convention: a
// section starts with a newline and a heading).
type factsBlock struct{ b strings.Builder }

func (f *factsBlock) add(s string) {
	s = strings.Trim(s, "\n")
	if s == "" {
		return
	}
	f.b.WriteString("\n" + s + "\n")
}

func (f *factsBlock) String() string { return f.b.String() }

// withLateSystem appends a conditional system rule (one that exists only for
// some MRs of a repository) after every static part of the system prompt,
// including the models note: the static prefix is the same with and without
// it.
func withLateSystem(req core.RunRequest, s string) core.RunRequest {
	req.SystemPrompt += s
	return req
}
