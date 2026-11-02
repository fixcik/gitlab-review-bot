package verify

import (
	"fmt"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
)

// BenchRequest is the verification run of `reviewbot bench` (ReviewBench): the
// same prompts and schema as the verification before publishing, for findings
// without a merge request (no merge base export, no diff excerpts, no MCP).
// parse maps the position of a finding in fs to its verdict; a finding the
// answer has no verdict for is not in the map.
func BenchRequest(dir, base, head, diff string, fs []output.Finding, timeout time.Duration) (req core.RunRequest, parse func(string) (map[int]FindingVerdict, error), err error) {
	items := make([]Item, len(fs))
	ids := map[string]bool{}
	index := map[string]int{}
	for i, f := range fs {
		id := fmt.Sprintf("F%d", i+1)
		items[i] = Item{ID: id, Index: i, F: f}
		ids[id], index[id] = true, i
	}
	sys, prompt, schema, err := renderVerification(Input{
		Dir: dir, Base: base, From: base, Head: head, Lang: "English", Diff: diff,
		Findings: items, BlockRule: prompts.BlockRule("major"),
	})
	if err != nil {
		return core.RunRequest{}, nil, err
	}
	req = core.RunRequest{Dir: dir, Prompt: prompt, SystemPrompt: sys, JSONSchema: schema, Timeout: timeout}
	parse = func(s string) (map[int]FindingVerdict, error) {
		out, err := parseVerifyOutput(s, 0, ids)
		if err != nil {
			return nil, err
		}
		m := map[int]FindingVerdict{}
		for _, v := range out.Findings {
			if i, ok := index[v.ID]; ok {
				m[i] = v
			}
		}
		return m, nil
	}
	return req, parse, nil
}
