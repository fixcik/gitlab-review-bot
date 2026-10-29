// Package verify is the adversarial verification run before publishing
// (review.verify). After the review answer is parsed, the migration rule
// applied (migcheck.Feature.Enforce) and linter duplicates dropped, ONE extra
// Claude run tries to refute:
//
//   - the NEW findings (not open bot threads, not linter duplicates) whose
//     severity is in review.verify.severities (default critical, major), at
//     most review.verify.max_findings of them, most severe first; the rest
//     are published unverified and the summary says so. Verdict per finding:
//     confirm | downgrade (strictly lower severity) | reject. Findings the
//     bot already proved deterministically are not sent (verify/verify_proven.go);
//   - Claude's new breaking-change claims (review.compat_check,
//     review/compatcheck: compatcheck.Candidates): confirmed | rejected,
//     exactly as before the merge of the two runs (verify/claims.go).
//
// Only breaking-change claims -> the run is byte for byte the previous
// compat verification run (prompt, system prompt, schema, no MCP). With
// findings the run gets a combined prompt and schema ("findings" for the
// findings, "verdicts" for the claims) and, with review.mcp on, a separate
// read-only MCP session (diff lines, API changes, linter results, coverage,
// system map, migrations, task, related repositories) opened through Tools,
// which the orchestrator gets from the review's MCP run (review/mcp).
//
// Apply: reject -> not published (recorded as outcome rejected_by_verifier
// for the stats, through review/learning); downgrade -> the new severity (it
// may stop blocking); confirm -> unchanged. Defense in depth: verdicts for
// unknown ids, upgrades and "downgrades" to the same or a higher level are
// ignored (confirm); a finding without a verdict is confirmed.
//
// Failure (global limit, Claude limit, invalid answer twice, error): the
// findings are published as they are, the summary block says why, a
// verify_failed problem is recorded; breaking-change claims are published
// "не перепроверено" and still block (as before).
//
// Budget: the run is a later run of the job: the global limit is checked
// first and env.RunStructured records it (the job's booking was committed by
// the review run). Token accounting: kind "verification".
//
// The package depends on review/env, review/learning (outcomes and project
// memory), review/linters, review/compatcheck and review/migcheck (their
// runs) and the leaves; the orchestrator calls Feature.BeforePublish and
// Feature.RecordRejected from the review flow.
package verify

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Observer receives the verification metrics
// (reviewbot_verify_runs_total, reviewbot_verify_verdicts_total);
// implemented by *telemetry.Metrics. review.Deps.Verify may be nil.
type Observer interface {
	ObserveVerifyRun(result string)
	ObserveVerifyVerdict(verdict, severity string)
}

// Verification run results (metric label) and verdicts.
const (
	resultOK      = "ok"
	resultLimit   = "limit"
	resultClaude  = "rate_limited"
	resultInvalid = "invalid_output"
	resultError   = "error"

	VerdictConfirm   = "confirm"
	verdictDowngrade = "downgrade"
	verdictReject    = "reject"
	// Metric only: no verdict (counted as confirm) / the run failed.
	verdictMissing    = "missing"
	verdictUnverified = "unverified"
	// verdictProven: not sent, the bot proved it (verify/verify_proven.go).
	verdictProven = "proven"
	// severityBreaking is the metric severity of breaking-change claims.
	severityBreaking = "breaking"
)

// Feature checks the findings and breaking-change claims of a review
// before they are published.
type Feature struct {
	env      *env.Env
	learning *learning.Feature // outcomes of rejected findings
	// obs: verification metrics; may be nil.
	obs Observer
}

// New returns the verification on the shared review env; lf records the
// outcomes of rejected findings; obs may be nil.
func New(e *env.Env, lf *learning.Feature, obs Observer) *Feature {
	return &Feature{env: e, learning: lf, obs: obs}
}

// Item is a finding sent to the verification run.
type Item struct {
	ID    string // F1, F2, ... (prompt and answer)
	Index int    // position in ReviewOutput.Findings
	F     output.Finding
	// Excerpt is the diff of the finding's file around its lines ("" when
	// the reviewed diff has none).
	Excerpt string
}

// verifyChange is a rejected or downgraded finding (summary, outcomes).
type verifyChange struct {
	F        output.Finding // as reported
	To       core.Severity
	Reason   string
	Evidence string
}

// Run is the verification of this review's findings.
type Run struct {
	items    []Item
	overflow int // eligible findings over max_findings (published unverified)
	// proven: eligible findings the bot proved deterministically
	// (verify/verify_proven.go), published without the verification.
	proven []provenFinding
	failed string // why the run could not verify them ("" = it did)

	confirmed  int
	downgraded []verifyChange
	rejected   []verifyChange
}

// FindingVerdict is the run's answer for one finding.
type FindingVerdict struct {
	ID       string `json:"id"`
	Verdict  string `json:"verdict"`
	Severity string `json:"severity,omitempty"`
	Reason   string `json:"reason"`
	Evidence string `json:"evidence,omitempty"`
}

// Output is the combined answer: "verdicts" for breaking-change
// claims (output.CompatVerification), "findings" for findings.
type Output struct {
	Verdicts []output.CompatVerdict `json:"verdicts"`
	Findings []FindingVerdict       `json:"findings"`
}

// Input is what one verification run checks. BeforePublish fills Compat,
// CompatOn, Findings and BlockRule; they are set directly only to render the
// prompts alone (tests).
type Input struct {
	Dir, Base, From, Head, Lang string
	// BaseDir / BaseRoot: the merge base export (env/base.go), host path and
	// the path Claude sees; "" = none.
	BaseDir, BaseRoot string
	Diff              string // the reviewed diff (finding excerpts)
	CtxDirs           []core.ContextDir
	apiCtx            string // OpenAPI changelog for the claims
	Compat            []compatcheck.Item
	// CompatOn: the repository has the compat check on (the review's
	// compatcheck.Run exists). The findings run then always carries the claim
	// rules and a required "verdicts" (empty without claims), so its system
	// prompt and schema do not depend on whether this MR has claims
	// (prompt caching).
	CompatOn  bool
	Findings  []Item
	BlockRule string
	ms        Tools // the review's MCP (read tools); nil = off
	// Migrations / Lint: the review's migrations and linter results
	// (findings they prove are not verified, verify/verify_proven.go); nil =
	// none.
	Migrations *migcheck.Run
	Lint       *linters.Run
}

// Tools opens the verifier's read-only MCP session over the review's data
// (implemented by the review's MCP run, review/mcp: Run.VerifyTools).
type Tools interface {
	// OpenVerify opens the session for a run of the given timeout; the
	// session is never nil (without tools its Server is nil).
	OpenVerify(ctx context.Context, timeout time.Duration) Session
}

// Session is the verifier's MCP session.
type Session interface {
	Close()
	Server() *core.MCPServer
	SystemNote() string
}

// verifyResult is the outcome of one verification run.
type verifyResult struct {
	compatPublish  []compatcheck.Item
	compatRejected []compatcheck.Rejected
	verdicts       map[string]FindingVerdict // by item id
	failed         string                    // findings reason ("" = ran)
}

// BeforePublish runs the ONE verification run of a review (findings
// and breaking-change claims) and applies its verdicts to out and cr. It
// returns the findings part (nil when the verification of findings is off
// for the repository).
func (vf *Feature) BeforePublish(ctx context.Context, job core.Job, eff config.Effective, cr *compatcheck.Run, out *output.ReviewOutput,
	threads *mrthread.Threads, ms Tools, in Input) *Run {
	ctx, tsp := tracing.Start(ctx, "review.verify")
	defer tsp.End()
	var vr *Run
	if eff.Verify {
		vr = &Run{}
		vr.items, vr.overflow, vr.proven = vf.selectFindings(out.Findings, threads, in.Diff, provenBy(in.Migrations.ProvenIDs(), in.Lint, cr))
		for _, p := range vr.proven {
			vf.observeVerdict(verdictProven, string(p.F.Severity))
		}
		in.Findings = vr.items
	}
	in.Compat, in.apiCtx = compatcheck.Candidates(cr, out, threads)
	in.ms, in.BlockRule, in.CompatOn = ms, prompts.BlockRule(vf.env.Cfg.Review.BlockMinSeverity), cr != nil
	res := vf.runVerification(ctx, job, in)
	cr.AddVerified(res.compatPublish, res.compatRejected)
	if vr != nil {
		vr.failed = res.failed
		vf.applyFindingVerdicts(job, out, vr, res)
	}
	return vr
}

// runVerification makes the verification run for the claims and findings
// of in (nothing when both are empty).
func (vf *Feature) runVerification(ctx context.Context, job core.Job, in Input) (res verifyResult) {
	if len(in.Compat) == 0 && len(in.Findings) == 0 {
		return res
	}
	log := vf.env.Log.With("mr", job.MRKey())
	fail := func(result, why string, err error) verifyResult {
		return vf.verificationFailed(ctx, in, result, why, err)
	}
	dec, err := vf.env.Limiter.CheckGlobal(ctx)
	if err != nil {
		log.Warn("review: verification: global limit check", "err", err)
		return fail(resultError, whyError, err)
	}
	if !dec.Allowed {
		log.Info("review: verification skipped: global limit", "reason", dec.Reason)
		return fail(resultLimit, whyLimit, errors.New(dec.Reason))
	}
	sys, prompt, schema, err := renderVerification(in)
	if err != nil {
		log.Error("review: render verification prompt", "err", err)
		return fail(resultError, whyError, err)
	}
	sys, prompt = sys+learning.VerifyMemoryNote(ctx), prompt+learning.VerifyMemoryBlock(ctx) // project memory (learning/memory.go)
	req := core.RunRequest{
		Dir: in.Dir, Prompt: prompt, SystemPrompt: sys, BaseDir: in.BaseDir,
		JSONSchema: schema, Timeout: vf.verifyTimeout(),
		ContextDirs: in.CtxDirs,
	}
	if len(in.Findings) > 0 && in.ms != nil {
		// MCP read tools for the verifier (a session of its own: the
		// review's session is closed when the review run ends).
		vms := in.ms.OpenVerify(ctx, vf.verifyTimeout())
		defer vms.Close()
		req.MCP = vms.Server()
		req.SystemPrompt += vms.SystemNote()
	}
	req = vf.env.WithModels(req)
	if m := strings.TrimSpace(vf.env.Cfg.Review.Verify.Model); m != "" {
		req.Model = m
	}
	ids := map[string]bool{}
	for _, it := range in.Findings {
		ids[it.ID] = true
	}
	var v *Output
	// Charge the verification pass as its own kind, then back to review.
	jobctx.AttributeKind(ctx, meter.KindVerification)
	defer jobctx.AttributeKind(ctx, meter.KindReview)
	err = vf.env.RunStructured(ctx, job, req, func(s string) error {
		o, err := parseVerifyOutput(s, len(in.Compat), ids)
		if err == nil {
			v = o
		}
		return err
	})
	if err != nil {
		var rl *core.ErrClaudeRateLimited
		var inv *env.InvalidOutputError
		switch {
		case errors.As(err, &rl):
			log.Warn("review: verification: claude rate limited", "msg", rl.Message)
			return fail(resultClaude, whyClaude, err)
		case errors.As(err, &inv):
			log.Warn("review: verification: invalid output", "err", err)
			return fail(resultInvalid, whyInvalid, err)
		default:
			log.Warn("review: verification failed", "err", err)
			return fail(resultError, whyError, err)
		}
	}
	vf.observeRun(resultOK)
	return vf.verificationDone(log, in, ids, v)
}

// verificationFailed is the result of a run that could not verify in: the
// claims are published unverified, the findings as they are.
func (vf *Feature) verificationFailed(ctx context.Context, in Input, result, why string, err error) verifyResult {
	vf.observeRun(result)
	for _, it := range in.Findings {
		vf.observeVerdict(verdictUnverified, string(it.F.Severity))
	}
	for range in.Compat {
		vf.observeVerdict(verdictUnverified, severityBreaking)
	}
	if ctx.Err() == nil {
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		jobctx.BotProblem(ctx, problems.VerifyFailed, "перепроверка: "+why, detail)
	}
	return verifyResult{failed: why, compatPublish: unverifiedItems(in.Compat, compatUnverifiedReason(result))}
}

// verificationDone splits the run's answer v into the claims to publish or
// reject and the verdicts by finding id (the first one of a known id).
func (vf *Feature) verificationDone(log *slog.Logger, in Input, ids map[string]bool, v *Output) (res verifyResult) {
	res.compatPublish, res.compatRejected = compatVerdicts(in.Compat, v.Verdicts)
	for _, it := range res.compatPublish {
		if it.Unverified {
			vf.observeVerdict(verdictMissing, severityBreaking)
		} else {
			vf.observeVerdict(VerdictConfirm, severityBreaking)
		}
	}
	for range res.compatRejected {
		vf.observeVerdict(verdictReject, severityBreaking)
	}
	res.verdicts = map[string]FindingVerdict{}
	for _, fv := range v.Findings {
		if !ids[fv.ID] {
			log.Info("review: verification: verdict for an unknown finding ignored", "id", mdtext.Shorten(fv.ID, 20))
			continue
		}
		if _, dup := res.verdicts[fv.ID]; !dup {
			res.verdicts[fv.ID] = fv
		}
	}
	log.Info("review: verification done", "findings", len(in.Findings), "claims", len(in.Compat), "claims_rejected", len(res.compatRejected))
	return res
}

func (vf *Feature) verifyTimeout() time.Duration {
	if d := vf.env.Cfg.Review.Verify.Timeout.D(); d > 0 {
		return d
	}
	return vf.env.Cfg.Claude.Timeout.D()
}

func (vf *Feature) observeRun(result string) {
	if vf.obs != nil {
		vf.obs.ObserveVerifyRun(result)
	}
}

func (vf *Feature) observeVerdict(verdict, severity string) {
	if vf.obs != nil {
		vf.obs.ObserveVerifyVerdict(verdict, severity)
	}
}

// Why the findings were not verified (Russian, summary and problem); the
// limits read like the breaking-change threads' reasons (verify/claims.go).
const (
	whyLimit   = unverifiedLimit
	whyClaude  = unverifiedClaude
	whyInvalid = "ответ перепроверки не по схеме"
	whyError   = "ошибка запуска (подробности в логах бота)"
)

// compatUnverifiedReason keeps the reasons breaking-change threads always
// showed (verify/claims.go).
func compatUnverifiedReason(result string) string {
	switch result {
	case resultLimit:
		return unverifiedLimit
	case resultClaude:
		return unverifiedClaude
	default:
		return unverifiedError
	}
}
