package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/descsum"
	"github.com/fixcik/gitlab-review-bot/internal/review/labels"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/mcp"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
)

// preparePrompt collects the prompt inputs that are not analyzer results.
// The returned context carries the run's memory and skills.
func (r *Reviewer) preparePrompt(ctx context.Context, rv *reviewRun) context.Context {
	// Team rules learned from earlier discussions (learning/rules_prompt.go).
	rv.rules = r.learning.PromptRules(ctx, rv.job, rv.eff)
	// Path-specific instructions matching the reviewed files (paths.go).
	rv.paths = matchPathInstructions(rv.eff.PathInstructions, rv.files)
	// Labels the model decides on (labels/prompt.go; nothing without instruction rules).
	rv.lp = labels.NewPrompt(rv.eff.Labels, rv.incremental)
	// Project memory (learning/memory.go): prompt section, system rules, schema.
	rv.mem, ctx = r.learning.PrepareMemory(ctx, rv.job, rv.eff)
	// Team skills (skills.go): staged read-only copy + system prompt list.
	rv.sk, ctx = r.prepareSkills(ctx, rv.job, rv.eff)
	return ctx
}

// reviewRequest renders the review request with the MCP tools of ms (nil or
// dropped = without them, the full prompt sections) and the parser of its
// answer (it sets rv.out). The MCP fallback (mcp/fallback.go) builds it a
// second time without the tools when the CLI did not connect.
func (r *Reviewer) reviewRequest(ctx context.Context, rv *reviewRun, ms *mcp.Run) (core.RunRequest, func(string) error, error) {
	sys, err := r.reviewSystemPrompt(rv, ms)
	if err != nil {
		return core.RunRequest{}, nil, err
	}
	prompt, err := r.reviewUserPrompt(rv, ms)
	if err != nil {
		return core.RunRequest{}, nil, err
	}
	// expand/contract and the severity policy (migcheck/migrations.go): last in the
	// system prompt.
	migSys := ""
	if rv.mg != nil {
		migSys = rv.mg.System
	}
	req := withLateSystem(r.withRoute(rv.rt, core.RunRequest{
		Dir: rv.co.Dir(), Prompt: prompt, SystemPrompt: sys, BaseDir: rv.bs.Dir,
		JSONSchema: r.reviewSchema(rv, ms), Timeout: r.env.Cfg.Claude.Timeout.D(),
		ContextDirs: rv.rc.Dirs, SkillsDir: rv.sk.dir, MCP: ms.Server(),
	}), migSys)
	return req, func(s string) error { return r.parseReview(ctx, rv, ms, s) }, nil
}

// reviewSystemPrompt is the system prompt of the review run. Per-MR facts
// never go here (prompt caching: prompt_cache.go), they end the user prompt.
func (r *Reviewer) reviewSystemPrompt(rv *reviewRun, ms *mcp.Run) (string, error) {
	sys, err := prompts.Render(reviewSystemTmpl, prompts.System{Language: rv.lang, BaseDir: rv.bs.Root, BlockRule: prompts.BlockRule(r.env.Cfg.Review.BlockMinSeverity)})
	if err != nil {
		return "", fmt.Errorf("render system prompt: %w", err)
	}
	if rv.cr != nil {
		sys += compatcheck.SystemRules
	}
	sys += crossrepo.DiagramRules(rv.eff, rv.lang)           // crossrepo/diagram.go
	sys += descsum.Rules(rv.eff.DescriptionSummary, rv.lang) // descsum: the summary in the MR description
	sys += learning.RulesSystemNote(rv.rules) + pathSystemNote(rv.eff.PathInstructions) + rv.lp.System + rv.mem.System + rv.sk.system
	sys += pipeline.TBDRulesNote(rv.eff.TBD) // feature-flag rule (pipeline/tbd.go)
	sys += ms.SystemNote()                   // MCP tools, submit_finding workflow (mcp/prompt.go)
	// What the model could not verify (problems.go): report_problem with
	// MCP, else the optional "problems" array of the answer.
	sys += r.problemsSystemNote(ms.Server() != nil)
	return sys, nil
}

// reviewSchema is the JSON schema of the review answer.
func (r *Reviewer) reviewSchema(rv *reviewRun, ms *mcp.Run) string {
	schema := task.ReviewSchemaFor(rv.tc.AskTask(), rv.tc.AskTitle, r.task.TitleTypes())
	if rv.cr != nil {
		schema = compatcheck.WithSchema(schema)
	}
	schema = crossrepo.DiagramSchema(schema, rv.eff.Diagram) // crossrepo/diagram.go
	schema = descsum.WithSchema(schema, rv.eff.DescriptionSummary)
	schema = rv.mem.WithSchema(rv.lp.WithSchema(schema))
	if rv.mg != nil {
		schema = migcheck.WithSchema(schema) // findings[].deployed_usage
	}
	if r.problemsOn() && ms.Server() == nil {
		schema = withProblemsSchema(schema)
	}
	return schema
}

// reviewUserPrompt is the user prompt of the review run.
func (r *Reviewer) reviewUserPrompt(rv *reviewRun, ms *mcp.Run) (string, error) {
	data := reviewData{
		Title: rv.mr.Title, Description: rv.mr.Description,
		From: rv.from, To: rv.head, Incremental: rv.incremental,
		Diff: rv.capped.Diff, Truncated: rv.capped.Truncated, OmittedFiles: rv.capped.Omitted,
		Files: rv.files, Instructions: strings.TrimSpace(rv.eff.Instructions),
		BaseDir: rv.bs.Root, Language: rv.lang,
		RulesBlock:   learning.RenderRulesBlock(rv.rules) + renderPathBlock(rv.paths) + rv.lp.Block,
		ContextBlock: rv.rc.Block + rv.mem.Block,
	}
	for _, id := range rv.threads.OpenOrder {
		f := rv.threads.Open[id]
		data.OpenFindings = append(data.OpenFindings, prompts.OpenFinding{ID: id, Path: f.Path, Line: f.Line, Title: f.Title, Body: mdtext.Shorten(f.Body, 1500)})
	}
	var err error
	if data.TaskBlock, err = r.task.RenderPrompt(rv.tc, rv.bs.Root); err != nil {
		return "", fmt.Errorf("render task prompt: %w", err)
	}
	if rv.mg != nil {
		data.MigrationsBlock = ms.MigrationsPrompt(rv.mg)
	}
	if data.CompatBlock, err = compatcheck.RenderPrompt(rv.cr, rv.threads); err != nil {
		return "", fmt.Errorf("render compat prompt: %w", err)
	}
	data.CompatBlock += ms.APIPrompt(rv.api, rv.cr != nil, r.env.Cfg.Claude.SearchModel != "")
	data.CompatBlock += ms.LintPrompt(rv.lr)
	// Per-MR facts: the end of the user prompt (prompt_cache.go).
	var facts factsBlock
	facts.add(ms.CoverageNote(pipeline.CoverageForClaude(rv.cov), rv.cov))
	if rv.sm != nil {
		facts.add(ms.SysmapSystem(rv.sm)) // usages to verify first (crossrepo/sysmap.go)
	}
	if rv.smDeps != nil {
		facts.add(rv.smDeps.System) // phase B facts, findings at most major (crossrepo/sysmapdeps.go)
	}
	data.FactsBlock = facts.String()
	prompt, err := prompts.Render(reviewUserTmpl, data)
	if err != nil {
		return "", fmt.Errorf("render prompt: %w", err)
	}
	return prompt, nil
}

// parseReview parses the answer of the review run into rv.out.
func (r *Reviewer) parseReview(ctx context.Context, rv *reviewRun, ms *mcp.Run, s string) error {
	o, err := parseReviewOutput(s, r.env.Cfg.Review.MaxFindings)
	if err != nil {
		return err
	}
	o.Findings = ms.Merge(o.Findings) // findings submitted through MCP
	if r.problemsOn() && ms.Server() == nil {
		addModelProblems(ctx, s) // the answer's "problems" (problems.go)
	}
	task.NormalizeOutput(o, rv.tc, r.task.TitleTypes())
	rv.rc.LinkReview(o)
	compatcheck.NormalizeOutput(o, rv.cr != nil)
	descsum.Normalize(o, rv.eff.DescriptionSummary)
	rv.lp.Parse(s) // the answer's "labels" (labels/prompt.go)
	rv.out = o
	return nil
}
