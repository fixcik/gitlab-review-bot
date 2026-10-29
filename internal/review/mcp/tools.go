package mcp

// The session's tool set (Run.Tools), the shared argument helpers and
// report_progress.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
)

// Tool names (mcp__reviewbot__<name> for the CLI).
const (
	toolDiffLines  = "diff_lines"
	toolAPIChanges = "api_changes"
	toolLint       = "lint_findings"
	toolCoverage   = "coverage"
	toolMapLookup  = "map_lookup"
	toolMigrations = "migrations"
	toolTask       = "task"
	toolRelated    = "related_repos"
	toolSubmit     = "submit_finding"
	toolWithdraw   = "withdraw_finding"
	toolList       = "list_findings"
	toolProgress   = "report_progress"
)

const (
	pathOnlySchema = `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","description":"File path relative to the repository root; omit for all files."}}}`
	noArgsSchema   = `{"type":"object","additionalProperties":false,"properties":{}}`
	// maxListed caps list answers (files, entries).
	maxListed  = 200
	maxPathArg = 512
)

type pathArg struct {
	Path string `json:"path"`
}

// cleanPathArg validates an optional path argument.
func cleanPathArg(raw json.RawMessage) (string, error) {
	var a pathArg
	if err := mcpserver.DecodeArgs(raw, &a); err != nil {
		return "", err
	}
	p := finding.CleanPath(a.Path)
	if p == "" {
		return "", nil
	}
	if len(p) > maxPathArg || !migcheck.CleanRepoPath(p) {
		return "", fmt.Errorf("path must be relative to the repository root, without \"..\" (got %q)", mdtext.Shorten(a.Path, 80))
	}
	return p, nil
}

// Tools are the tools of the run's session: diff_lines always, a read tool
// for each analyzer result the run has, the findings tools, report_progress
// and report_problem (review.problems.enabled).
func (m *Run) Tools() []mcpserver.Tool {
	in := m.in
	ts := []mcpserver.Tool{{
		Name:        toolDiffLines,
		Description: "Changed lines of a file in this merge request's diff: added line ranges and all commentable (added + context) ranges in the NEW file. A finding is posted inline only on a commentable line; prefer added lines. Without a path: every file of the diff.",
		InputSchema: json.RawMessage(pathOnlySchema), ReadOnly: true, Handler: m.diffLines,
	}}
	if compatcheck.RenderAPIPrompt(in.API, in.CompatOn, false) != "" {
		ts = append(ts, mcpserver.Tool{Name: toolAPIChanges, ReadOnly: true, InputSchema: json.RawMessage(noArgsSchema),
			Description: "The API changelog of this merge request: what changed in its OpenAPI / Swagger specs against the merge base (oasdiff), grouped by endpoint, ⛔ breaking / ⚠️ potentially breaking / ➕ addition / ℹ️ other.",
			Handler: func(context.Context, json.RawMessage) (string, error) {
				return compatcheck.RenderAPIPrompt(in.API, in.CompatOn, m.f.env.Cfg.Claude.SearchModel != ""), nil
			}})
	}
	if in.Lint != nil && in.Lint.Res != nil && len(in.Lint.Res.Findings) > 0 {
		ts = append(ts, mcpserver.Tool{Name: toolLint, ReadOnly: true, InputSchema: json.RawMessage(pathOnlySchema),
			Description: "Linter and security scanner results on the lines this merge request changes (the bot publishes them itself; do not report them again). Optional path filter.",
			Handler:     m.lintFindings})
	}
	if cr := in.Coverage; cr != nil && cr.State == pipeline.CoverageOK {
		ts = append(ts, mcpserver.Tool{Name: toolCoverage, ReadOnly: true, InputSchema: json.RawMessage(pathOnlySchema),
			Description: "Test coverage of the changed lines from the CI coverage report: covered / total changed executable lines and the uncovered line ranges per file. Optional path filter (substring).",
			Handler:     m.coverage})
	}
	if (in.Sysmap != nil && in.Sysmap.Impact != nil && len(in.Sysmap.Impact.Contracts) > 0) || (in.SysmapDeps != nil && in.SysmapDeps.System != "") || m.mapIndex() != nil {
		ts = append(ts, mcpserver.Tool{Name: toolMapLookup, ReadOnly: true,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"query":{"type":"string","description":"A contract (\"POST /orders\", \"/orders/{id}\", an event name such as order.paid) or a file path. Empty: the contracts this merge request touches and the dependency facts (phase B)."}}}`),
			Description: "The bot's system map of contracts between the team's repositories (HTTP endpoints, webhooks, WebSocket, SSE): who provides and who consumes a contract, with file:line locations and where the repositories are mounted; plus this merge request's touched contracts and dependency facts. Only this repository and the repositories this review may read.",
			Handler:     m.mapLookup})
	}
	if in.Migrations != nil {
		ts = append(ts, mcpserver.Tool{Name: toolMigrations, ReadOnly: true, InputSchema: json.RawMessage(noArgsSchema),
			Description: "The migration hints of this merge request: its migration files, operations the scanner flagged as often unsafe during a rolling deploy, and text-search candidates (merge base and head) for the names they drop or rename.",
			Handler:     func(context.Context, json.RawMessage) (string, error) { return in.Migrations.Prompt, nil }})
	}
	if in.Task != nil && in.Task.AskTask() {
		ts = append(ts, mcpserver.Tool{Name: toolTask, ReadOnly: true, InputSchema: json.RawMessage(noArgsSchema),
			Description: "The Jira task(s) of this merge request (summary, description excerpt, parent / epic, comments) as the prompt shows them.",
			Handler: func(context.Context, json.RawMessage) (string, error) {
				return m.f.task.RenderPrompt(in.Task, in.Base)
			}})
	}
	if in.Repos != nil && len(in.Repos.Dirs) > 0 {
		ts = append(ts, mcpserver.Tool{Name: toolRelated, ReadOnly: true, InputSchema: json.RawMessage(noArgsSchema),
			Description: "The related and context repositories this review may read and the read-only paths where they are mounted (/repos/<group>/<project>).",
			Handler:     m.relatedRepos})
	}
	ts = append(ts,
		mcpserver.Tool{Name: toolSubmit, InputSchema: m.submitSchema(), Handler: m.submit,
			Description: "Submit one review finding. The bot validates it immediately with the rules it applies when publishing and answers {status: accepted | adjusted | rejected, id, reasons, hint}. Fix and resubmit rejected findings (the same path and title replace the earlier version). Accepted findings are published when the review ends."},
		mcpserver.Tool{Name: toolWithdraw, Handler: m.withdraw,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["id"],"properties":{"id":{"type":"string","description":"Finding id from submit_finding, e.g. F3."}}}`),
			Description: "Withdraw a submitted finding by its id (it will not be published)."},
		mcpserver.Tool{Name: toolList, ReadOnly: true, InputSchema: json.RawMessage(noArgsSchema), Handler: m.list,
			Description: "List the findings submitted so far (id, path, line, severity, title)."},
		mcpserver.Tool{Name: toolProgress, Handler: m.progress,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["stage"],"properties":{"stage":{"type":"string","maxLength":60,"description":"Short stage name, e.g. reading-diff, checking-consumers, writing-answer."},"note":{"type":"string","maxLength":300}}}`),
			Description: "Optional: record the review stage you are at (bot diagnostics only; nothing is posted)."},
	)
	if m.f.env.Cfg.Review.Problems.Enabled { // review.problems.enabled
		ts = append(ts, m.reportProblemTool()) // what the model could not verify
	}
	return ts
}

type progressArgs struct {
	Stage string `json:"stage"`
	Note  string `json:"note"`
}

// progress is report_progress: a log line, nothing is posted.
func (m *Run) progress(_ context.Context, raw json.RawMessage) (string, error) {
	var a progressArgs
	if err := mcpserver.DecodeArgs(raw, &a); err != nil {
		return "", err
	}
	stage := mdtext.Shorten(a.Stage, 60)
	if stage == "" {
		return "", fmt.Errorf("stage is required")
	}
	m.log.Info("review: mcp progress", "stage", stage, "note", mdtext.Shorten(a.Note, 300))
	return `{"status":"recorded"}`, nil
}
