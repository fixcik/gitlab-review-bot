// Package mcp serves the review bot's own tools to Claude over MCP
// (internal/mcpserver; review.mcp) during a review run. When the server is
// on, every review gets a session with:
//
//   - read tools that hand out what this run's analyzers already computed
//     (diff lines, API changelog, linter results, coverage, system map,
//     migration hints, Jira task, related repositories; mcp/read.go,
//     maplookup.go), so the prompt carries one-line summaries instead of
//     the big sections (mcp/prompt.go);
//   - submit_finding / withdraw_finding / list_findings: the model submits
//     each finding as it goes; the bot validates it at once with the same
//     functions the publishing pipeline uses (placement in the diff,
//     migrations evidence, linter duplicates, open threads, the findings
//     cap; mcp/validate.go) and answers accepted / adjusted / rejected with
//     reasons. Accepted findings are kept in memory only (mcp/findings.go);
//   - report_progress: a stage line for the run's diagnostics, and
//     report_problem (review.problems) for what the model could not verify
//     (mcp/problems.go).
//
// Nothing here writes to GitLab. At the end the submitted findings are
// merged with the findings of the final JSON answer (Run.Merge) and the
// usual post-run pipeline runs unchanged (it enforces everything again).
// The verification run gets a read-only session over the same data
// (Run.VerifyTools, mcp/verify.go). When the CLI could not use the tools,
// the review runs once more without them and the server skips MCP for a
// while (Feature.RunReview, mcp/fallback.go). With review.mcp off (or
// mcp.enabled: false in the repository) none of this exists: Open returns
// a nil *Run, whose helpers all answer as without MCP, and the prompt is
// exactly what it was before.
//
// The package depends on env.Env (configuration, logger, GitLab client, the
// diff index, the Claude runs and the global limiter), publish.Publisher
// (where a finding will be placed), the features whose results the tools
// serve (task, linters, migcheck, crossrepo, compatcheck, pipeline) and
// jobctx (bot problems). It implements verify.Tools / verify.Session for
// the verifier and needs nothing from the orchestrator, which opens the
// session (Feature.Open), builds the prompt with Run's helpers, runs the
// review through Feature.RunReview and closes the session.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
)

// Inputs are the analyzer results of the run the tools serve (nil = that
// analyzer did not run).
type Inputs struct {
	Checkout core.Checkout
	// Files are the files of the reviewed diff (diff_lines without a path
	// when the MR diff index cannot list its files).
	Files      []string
	API        *compatcheck.APIRun
	CompatOn   bool // the compat check is on: ⛔ API changes are already threads
	Lint       *linters.Run
	Coverage   *pipeline.CoverageRun
	Sysmap     *crossrepo.SysmapRun
	SysmapDeps *crossrepo.DepsRun
	Migrations *migcheck.Run
	Task       *task.Context
	// Repos are the context repositories mounted for the run.
	Repos   *crossrepo.RunContext
	Threads *mrthread.Threads // the MR's bot threads (open findings)
	Lang    string            // the review language
	// Base is the merge base export as Claude sees it ("" = none).
	Base string
}

// Feature serves the bot's own review tools to Claude over MCP during a
// review run (mcp/mcp.go: the session; tools.go, read.go, maplookup.go:
// the tools; validate.go: submit_finding; fallback.go: the run without
// tools when the server fails).
type Feature struct {
	env *env.Env
	pub *publish.Publisher
	// The features whose results the tools serve.
	task       *task.Feature
	lint       *linters.Feature
	migrations *migcheck.Feature
	crossrepo  *crossrepo.Feature
	// srv: the MCP server for review tools; nil = off.
	srv *mcpserver.Server
}

// New is the MCP feature over the features whose results its tools serve;
// srv is the bot's MCP server (nil = MCP off).
func New(e *env.Env, pub *publish.Publisher, tk *task.Feature, lint *linters.Feature, mig *migcheck.Feature, xr *crossrepo.Feature, srv *mcpserver.Server) *Feature {
	return &Feature{env: e, pub: pub, task: tk, lint: lint, migrations: mig, crossrepo: xr, srv: srv}
}

// Run is the MCP side of one review run. All methods are nil-safe: a nil
// *Run means MCP is off and every prompt helper returns today's text.
type Run struct {
	f   *Feature
	job core.Job
	mr  *core.MergeRequest
	in  Inputs
	log *slog.Logger

	// idx is the MR diff (GitLab's line map) for placement checks; idxOK is
	// false when it could not be loaded.
	idx   env.DiffIndex
	idxOK bool

	sess *mcpserver.Session
	srv  *core.MCPServer // the RunRequest.MCP value; nil = no tools

	mu       sync.Mutex
	drafts   []draft
	seq      int
	ev       *migcheck.EvidenceChecker
	verdicts map[string]int
}

// sessionMargin is the session's TTL beyond two run timeouts (the run and
// env.RunStructured's retry).
const sessionMargin = 5 * time.Minute

// sessionTTL is the TTL of a session for runs of the given timeout (0 = the
// default 15 minutes): two attempts at most (env.RunStructured's retry)
// plus a margin; the session is closed as soon as the run is over anyway.
func sessionTTL(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	return 2*timeout + sessionMargin
}

// runID names the run's session: the MR and its head commit.
func (m *Run) runID() string {
	return m.job.MRKey() + "@" + mdtext.ShortSHA(m.mr.DiffRefs.HeadSHA)
}

// Open starts the MCP session of a review run (nil when MCP is off for
// it or the session cannot start; the review then runs as without MCP).
func (mf *Feature) Open(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, in Inputs) *Run {
	if mf.srv == nil || !eff.MCP {
		return nil
	}
	log := mf.env.Log.With("mr", job.MRKey())
	if on, until := mf.srv.Degraded(); on {
		// A recent run's CLI did not connect (mcp/fallback.go): this one
		// goes without the tools from the start instead of failing the
		// same way and running twice.
		log.Info("review: mcp: skipped after a recent fallback, running without the tools", "until", until.Format(time.RFC3339))
		return nil
	}
	url := mf.env.Cfg.MCPClientURL()
	if url == "" {
		log.Warn("review: mcp: no client URL (review.mcp.url / sandbox.egress_proxy), running without the tools")
		jobctx.BotProblem(ctx, problems.MCPFailed, "нет адреса MCP для песочницы", "задайте review.mcp.url или sandbox.egress_proxy")
		return nil
	}
	m := &Run{f: mf, job: job, mr: mr, in: in, log: log, verdicts: map[string]int{}}
	if in.Migrations != nil {
		m.ev = migcheck.NewEvidenceChecker(in.Checkout, in.Migrations)
	}
	m.loadIndex(ctx)
	sess, token, err := mf.srv.Open(mcpserver.Spec{
		ProjectID: job.ProjectID, MRIID: job.MRIID, HeadSHA: mr.DiffRefs.HeadSHA,
		RunID: m.runID(),
		Tools: m.Tools(),
		TTL:   sessionTTL(mf.env.Cfg.Claude.Timeout.D()), Ctx: ctx,
		Instructions: "Tools of the review bot for this merge request. Submit every finding with submit_finding and fix the ones it rejects.",
	})
	if err != nil {
		log.Warn("review: mcp: session not opened, running without the tools", "err", err)
		jobctx.BotProblem(ctx, problems.MCPFailed, "сессия MCP не открыта", err.Error())
		return nil
	}
	m.sess = sess
	m.srv = &core.MCPServer{Name: mcpserver.ServerName, URL: url, Token: token, Tools: sess.ToolNames()}
	log.Info("review: mcp session opened", "tools", strings.Join(m.srv.Tools, ","), "diff_index", m.idxOK)
	return m
}

// loadIndex reads GitLab's MR diff for placement checks (the same source
// publish.Index uses when publishing).
func (m *Run) loadIndex(ctx context.Context) {
	m.idx = env.EmptyIndex{}
	diffs, err := m.f.env.GitLab.GetMRDiffs(ctx, m.job.ProjectID, m.job.MRIID)
	if err != nil {
		m.log.Warn("review: mcp: get mr diffs, placement is checked only when publishing", "err", err)
		return
	}
	m.idx, m.idxOK = m.f.env.Index(diffs), true
}

// Server is the RunRequest.MCP value (nil without MCP).
func (m *Run) Server() *core.MCPServer {
	if m == nil {
		return nil
	}
	return m.srv
}

// Close ends the session: its token is refused from now on. Idempotent.
func (m *Run) Close() {
	if m == nil || m.sess == nil {
		return
	}
	m.mu.Lock()
	n, verdicts := len(m.drafts), fmt.Sprint(m.verdicts)
	m.mu.Unlock()
	m.log.Info("review: mcp session closed", "calls", m.sess.Calls(), "findings", n, "verdicts", verdicts)
	m.sess.Close()
	m.sess = nil
}

// Session is the run's MCP session (nil without MCP or once closed).
func (m *Run) Session() *mcpserver.Session {
	if m == nil {
		return nil
	}
	return m.sess
}

// Has reports whether the run's session has the tool.
func (m *Run) Has(tool string) bool {
	return m != nil && m.srv != nil && slices.Contains(m.srv.Tools, tool)
}
