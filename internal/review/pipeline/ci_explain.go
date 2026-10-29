package pipeline

// The explanation of a failed pipeline (review.ci.explain): ONE Claude run
// with the read-only tools over the checked-out code (working tree = the
// failing commit, /base = the code before it) and, in the prompt, the
// capped diff and the digest of the failed jobs (untrusted). Per job it
// answers the cause (this change, infrastructure, a flaky test, unknown),
// what happened, how to fix it and where (file:line). Budget: the global
// hourly limit is checked first and the run is recorded like every other
// Claude run (env.RunStructured); meter kind "ci".

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/citelink"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
)

// Causes of a failed job.
const (
	ciCauseMR      = "mr"
	ciCauseInfra   = "infra"
	ciCauseFlaky   = "flaky"
	ciCauseUnknown = "unknown"
)

// Results of an explanation run (reviewbot_ci_explain_runs_total{result}).
const (
	ciExplainOK        = "ok"
	ciExplainLimit     = "limit"
	ciExplainRateLimit = "rate_limited"
	ciExplainInvalid   = "invalid_output"
	ciExplainError     = "error"
	ciExplainDuplicate = "duplicate" // the same failure was explained already: no run
)

const (
	// ciMaxDiffBytes caps the diff in the prompt (the model reads the
	// files it needs).
	ciMaxDiffBytes = 60_000
	// ciMaxLocations caps the locations per job.
	ciMaxLocations = 5
	// ciMaxText caps summary and fix.
	ciMaxText = 1000
)

// ciLoc is a place in the code.
type ciLoc struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

// ciJobCause is the explanation of one failed job.
type ciJobCause struct {
	Job       string  `json:"job"`
	Cause     string  `json:"cause"`
	Summary   string  `json:"summary"`
	Fix       string  `json:"fix"`
	Locations []ciLoc `json:"locations"`
}

// ciExplanation is the answer for one pipeline.
type ciExplanation struct {
	Jobs []ciJobCause `json:"jobs"`
	// CulpritMR (main branch scope): the iid of the merged MR that most
	// likely broke the pipeline; 0 = none of them / unknown.
	CulpritMR int64 `json:"culprit_mr,omitempty"`
}

// cause returns the cause of job (unknown when not explained).
func (e *ciExplanation) cause(job string) string {
	if e != nil {
		for _, j := range e.Jobs {
			if j.Job == job {
				return j.Cause
			}
		}
	}
	return ciCauseUnknown
}

// onlyInfra: every explained job failed because of the infrastructure or a
// flaky test (nobody's change).
func (e *ciExplanation) onlyInfra() bool {
	if e == nil || len(e.Jobs) == 0 {
		return false
	}
	for _, j := range e.Jobs {
		if j.Cause != ciCauseInfra && j.Cause != ciCauseFlaky {
			return false
		}
	}
	return true
}

// ciCandidate is a merged MR that may have broken a main branch.
type ciCandidate struct {
	IID                     int64
	Title, Author, MergedBy string
}

// ciExplainInput is one explanation run.
type ciExplainInput struct {
	dir     string    // checkout (working tree = the failing commit)
	base    *env.Base // the code before it (/base)
	digest  *ciDigest
	diff    string
	cut     bool   // the diff was capped
	what    string // "merge request" or "main branch"
	title   string // MR title (untrusted; MR scope)
	lang    string
	targets []ciCandidate // main scope: merged MRs since the last green pipeline
}

// explainableJobs are the failed jobs the model explains (lint.ci jobs
// have their own threads).
func explainableJobs(d *ciDigest) []string {
	var out []string
	for _, jd := range d.Failed {
		if jd.Kind != ciKindLint {
			out = append(out, jd.Job.Name)
		}
	}
	return out
}

// parseCIExplanation keeps the entries of known jobs (first one wins),
// normalizes causes, cleans texts and locations. No entry for any job is
// invalid (the retry nudge follows). candidates, when non-nil, are the
// valid culprit iids.
func parseCIExplanation(s string, jobs []string, candidates map[int64]bool) (*ciExplanation, error) {
	var raw ciExplanation
	if err := jsonschema.UnmarshalStrict(s, &raw); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, j := range jobs {
		known[j] = true
	}
	out := &ciExplanation{}
	seen := map[string]bool{}
	for _, j := range raw.Jobs {
		j.Job = strings.TrimSpace(j.Job)
		if !known[j.Job] || seen[j.Job] {
			continue
		}
		seen[j.Job] = true
		switch j.Cause = strings.ToLower(strings.TrimSpace(j.Cause)); j.Cause {
		case ciCauseMR, ciCauseInfra, ciCauseFlaky:
		default:
			j.Cause = ciCauseUnknown
		}
		j.Summary = mdtext.DefuseMentions(shortenKeepLines(prompts.CleanModelText(j.Summary), ciMaxText))
		j.Fix = mdtext.DefuseMentions(shortenKeepLines(prompts.CleanModelText(j.Fix), ciMaxText))
		j.Locations = cleanCILocations(j.Locations)
		out.Jobs = append(out.Jobs, j)
	}
	if len(out.Jobs) == 0 && len(jobs) > 0 {
		return nil, errors.New("no explanation for any of the given jobs")
	}
	if candidates != nil && candidates[raw.CulpritMR] {
		out.CulpritMR = raw.CulpritMR
	}
	return out, nil
}

// cleanCILocations keeps at most ciMaxLocations valid repository-relative
// locations.
func cleanCILocations(in []ciLoc) []ciLoc {
	var locs []ciLoc
	for _, l := range in {
		if strings.HasPrefix(strings.TrimSpace(l.Path), "/") {
			continue // absolute (/base, /etc): not a file of the repository
		}
		p := finding.CleanPath(l.Path)
		if p == "" || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || path.Clean(p) != p ||
			strings.ContainsAny(p, "`\n\r<>") || l.Line < 0 {
			continue
		}
		locs = append(locs, ciLoc{Path: p, Line: l.Line})
		if len(locs) == ciMaxLocations {
			break
		}
	}
	return locs
}

// shortenKeepLines caps s at n runes keeping its line breaks.
func shortenKeepLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// explainCI makes the explanation run. result is one of the ciExplain*
// values (a Claude usage limit: ciExplainRateLimit; callers post the
// digest without the explanation and do not postpone).
func (pf *Feature) explainCI(ctx context.Context, job core.Job, in ciExplainInput) (expl *ciExplanation, result string, err error) {
	log := pf.env.Log.With("mr", job.MRKey(), "pipeline", in.digest.Pipeline.ID)
	jobs := explainableJobs(in.digest)
	if len(jobs) == 0 {
		return nil, "", nil
	}
	defer func() {
		pf.observeCIExplain(result)
		if result == ciExplainInvalid && ctx.Err() == nil {
			jobctx.BotProblem(ctx, problems.InvalidOutput, "ответ разбора упавшего пайплайна не по схеме", err.Error())
		}
	}()
	dec, err := pf.env.Limiter.CheckGlobal(ctx)
	if err != nil {
		return nil, ciExplainError, err
	}
	if !dec.Allowed {
		log.Info("review: ci: explanation skipped: global limit", "reason", dec.Reason)
		return nil, ciExplainLimit, errors.New(dec.Reason)
	}
	data := ciPromptData{What: in.what, SHA: in.digest.Pipeline.SHA, Title: mdtext.OneLine(in.title), Failures: renderCIDigestPrompt(in.digest),
		Diff: in.diff, Cut: in.cut, Language: in.lang, BaseDir: in.base.MountRoot(), Pipeline: in.digest.Pipeline.ID,
		JobCount: len(jobs), MR: in.title != "", Main: in.targets != nil, Targets: in.targets}
	sys, err := prompts.Render(ciSystemTmpl, data)
	if err != nil {
		return nil, ciExplainError, err
	}
	prompt, err := prompts.Render(ciUserTmpl, data)
	if err != nil {
		return nil, ciExplainError, err
	}
	var cands map[int64]bool
	if in.targets != nil {
		cands = map[int64]bool{}
		for _, c := range in.targets {
			cands[c.IID] = true
		}
	}
	// Charged as its own kind (/usage "Разбор CI").
	jobctx.AttributeKind(ctx, meter.KindCI)
	err = pf.env.RunStructured(ctx, job, core.RunRequest{
		Dir: in.dir, Prompt: prompt, SystemPrompt: sys, BaseDir: in.base.HostDir(),
		JSONSchema: ciSchema(in.targets != nil),
		// The light model, no advisor and no search subagent (cheap).
		Model: pf.env.Cfg.CIExplainModel(), Timeout: pf.env.Cfg.Claude.Timeout.D(),
	}, func(s string) error {
		e, err := parseCIExplanation(s, jobs, cands)
		if err == nil {
			expl = e
		}
		return err
	})
	if err != nil {
		var rl *core.ErrClaudeRateLimited
		var inv *env.InvalidOutputError
		switch {
		case errors.As(err, &rl):
			log.Warn("review: ci: explanation rate limited", "msg", rl.Message)
			return nil, ciExplainRateLimit, err
		case errors.As(err, &inv):
			log.Warn("review: ci: explanation invalid", "err", err)
			return nil, ciExplainInvalid, err
		}
		log.Warn("review: ci: explanation failed", "err", err)
		return nil, ciExplainError, err
	}
	log.Info("review: ci: explained", "jobs", len(expl.Jobs), "culprit", expl.CulpritMR)
	return expl, ciExplainOK, nil
}

// ciDiff is the capped diff base..HEAD of a checkout.
func (pf *Feature) ciDiff(ctx context.Context, co core.Checkout, base string, ignore []string) (string, bool) {
	if base == "" {
		return "", false
	}
	d, err := co.Diff(ctx, base, ignore)
	if err != nil {
		pf.env.Log.Warn("review: ci: diff", "err", err)
		return "", false
	}
	c := diffcap.Cap(d, 0, ciMaxDiffBytes)
	return c.Diff, c.Truncated || len(c.Omitted) > 0
}

func ciCauseLabel(c string) string {
	switch c {
	case ciCauseMR:
		return "🧩 из-за изменений"
	case ciCauseInfra:
		return "🏗 инфраструктура CI"
	case ciCauseFlaky:
		return "🎲 нестабильный тест"
	}
	return "❔ причина не ясна"
}

// renderCIExplanation renders the per-job explanations. Locations become
// `group/project:path:line` citations (links via the citation linker).
func renderCIExplanation(e *ciExplanation, projectPath string, links *citelink.Linker) string {
	var b strings.Builder
	for _, j := range e.Jobs {
		fmt.Fprintf(&b, "**%s** — %s", mdtext.InlineCode(j.Job), ciCauseLabel(j.Cause))
		var locs []string
		for _, l := range j.Locations {
			c := projectPath + ":" + l.Path
			if l.Line > 0 {
				c += fmt.Sprintf(":%d", l.Line)
			}
			locs = append(locs, mdtext.CodeSpan(c))
		}
		if len(locs) > 0 {
			b.WriteString(" · " + strings.Join(locs, ", "))
		}
		b.WriteString("\n\n")
		if j.Summary != "" {
			b.WriteString(j.Summary + "\n\n")
		}
		if j.Fix != "" {
			b.WriteString("**Как починить:** " + j.Fix + "\n\n")
		}
	}
	return links.Link(mdtext.DefuseComments(b.String()))
}
