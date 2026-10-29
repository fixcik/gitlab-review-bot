package review

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/reactions"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/descsum"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/labels"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mcp"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

// Deps are the reviewer's collaborators.
type Deps struct {
	// Moderator is the service-token client (Maintainer: reviewer
	// assignment and thread resolution, which the bot's Reporter token
	// cannot do). Optional; nil means no reviewer assignment and thread
	// resolution goes through GitLab (403 for a Reporter bot).
	Moderator env.Moderator
	GitLab    core.GitLab
	Repos     core.Repos
	Runner    core.Runner
	Store     core.Store
	Limiter   core.Limiter
	Config    *config.Service
	// Tasks fetches Jira issues; nil when Jira is not configured.
	Tasks task.Source
	// Learnings stores learned team rules (*store.SQLite); nil disables
	// learning. ScratchDir is where tool-less extraction runs get their
	// empty working directory (default <data_dir>/scratch).
	Learnings  learn.Store
	ScratchDir string
	// Optional record stores (*store.SQLite); each nil turns its feature
	// off. Typed fields, not type assertions on Store, so a store without a
	// method fails to compile in cmd. Outcomes: finding outcomes
	// (learning/outcomes*.go, needs Learnings); MRFlags: pause and the like
	// (commands_pause.go); CIMain: main branch state of the CI digest
	// (pipeline/ci_main.go); LabelStore: the bot's labels per MR
	// (labels/labels.go).
	Outcomes   learn.OutcomeStore
	MRFlags    MRFlags
	CIMain     pipeline.CIMainStore
	LabelStore labels.Store
	// ContextRepos provides read-only snapshots of the other repositories of
	// gitlab.groups; nil when the feature is off (see crossrepo/context.go).
	ContextRepos crossrepo.ContextRepos
	// Memory stores the per-project operational memory (*store.SQLite);
	// nil disables it (see learning/memory.go).
	Memory memory.Store
	// Compat runs the deterministic backward-compatibility checks; nil
	// means a compat.Checker on top of Runner (see compatcheck/compat.go).
	Compat compatcheck.Checker
	// Lint runs linters and security scanners; nil means a lint.Checker on
	// top of Runner (see linters/linters.go).
	Lint linters.Checker
	// SystemMap is the map of contracts between repositories; nil when
	// the feature is off (see crossrepo/sysmap.go).
	SystemMap crossrepo.SystemMap
	// MCP is the bot's MCP server (review tools, see package review/mcp); nil = off.
	MCP *mcpserver.Server
	// Reactions applies award emoji (see reactions.go); nil = no reactions.
	Reactions *reactions.Reactor
	// Labels is the bot's own client for MR labels, ServiceLabels the
	// service-token client (used when the bot's token gets 403); both nil =
	// no automatic labels (see labels/gitlab.go). OnLabel counts label changes
	// (action add|remove, result); may be nil.
	Labels        labels.Client
	ServiceLabels labels.Client
	OnLabel       func(action, result string)
	// Description is the bot's own client for the MR description summary
	// (review.description_summary), ServiceDescription the service-token
	// one (used on 403); both nil = no description summary (descsum).
	Description        descsum.Client
	ServiceDescription descsum.Client
	// Verify receives the verification metrics (verify/verify.go); nil = none.
	Verify verify.Observer
	// OnSkip counts reviews that deliberately ran no Claude, by reason:
	// "too_big" (bigmr.go), SkipNoRepoConfig / SkipRepoConfigInvalid
	// (repoconfig.go); may be nil.
	OnSkip func(reason string)
	// OnGitLabSaved counts GitLab reads answered from the job's memo
	// (glmemo.go) by op (get_mr, get_mr_diffs, list_discussions); may be nil.
	OnGitLabSaved func(op string)
	// OnRoute counts reviews by model route (route.go: light, normal, heavy); may
	// be nil.
	OnRoute func(route string)
	// Scrub removes secrets from job logs and test messages (diag scrubber
	// with the configured secrets; nil = the built-in patterns). CI receives
	// the CI failure metrics; may be nil. CI is read with the bot's token
	// only (pipeline/ci_digest.go: ciRead).
	Scrub func(string) string
	CI    pipeline.Observer
	// BuildIndex wraps diffmap.Build.
	BuildIndex func([]core.FileDiff) env.DiffIndex
	// RepoConfigTTL: how long .reviewbot.yaml reads are cached (0 =
	// DefaultRepoConfigTTL, negative = no cache).
	RepoConfigTTL time.Duration
	BotUserID     int64
	BotUsername   string
	Logger        *slog.Logger
	Now           func() time.Time
}

// Reviewer handles jobs. It is safe for concurrent use (the queue serializes
// jobs per MR).
type Reviewer struct {
	// env: what the features share (package review/env); pub: the notes and
	// threads of the bot (package review/publish).
	env *env.Env
	pub *publish.Publisher

	// The features (one package each, wired in New).
	task       *task.Feature        // Jira issue, MR title (review/task)
	learning   *learning.Feature    // team rules, outcomes, memory (review/learning)
	lint       *linters.Feature     // linters, scanners, CI linter reports, TODOs (review/linters)
	compat     *compatcheck.Feature // backward compatibility, API changelog (review/compatcheck)
	migrations *migcheck.Feature    // migration safety (review/migcheck)
	verify     *verify.Feature      // verification of findings before publishing (review/verify)
	pipeline   *pipeline.Feature    // CI failures, the TBD block, coverage (review/pipeline)
	crossrepo  *crossrepo.Feature   // related repositories, system map, diagrams (review/crossrepo)
	labels     *labels.Feature      // automatic MR labels (review/labels)
	descsum    *descsum.Feature     // the summary in MR descriptions (review/descsum)
	mcp        *mcp.Feature         // the bot's MCP tools of a review run (review/mcp)

	// rx: award emoji state (reactions*.go); nil = no reactions.
	rx *reactor
	// mrFlags: see Deps.MRFlags (commands_pause.go); may be nil.
	mrFlags MRFlags
	// onSkip: see Deps.OnSkip (bigmr.go, repoconfig.go); may be nil.
	onSkip func(reason string)
	// onRoute: see Deps.OnRoute (route.go); may be nil.
	onRoute func(route string)

	mu sync.Mutex
	// enqueue schedules a job (SetEnqueue, commands.go); guarded by mu.
	enqueue func(core.Job)

	// Repository config (repoconfig.go): read cache and its TTL.
	cfgCache cfgCache
	cfgTTL   time.Duration
}

// New builds a Reviewer.
func New(d Deps) *Reviewer {
	r := &Reviewer{
		// Per-job memo of MR reads (glmemo.go); the service token's writes
		// drop it too.
		env: &env.Env{
			GitLab: newMemoGitLab(d.GitLab, d.OnGitLabSaved), Mod: newMemoModerator(d.Moderator),
			Repos: d.Repos, Runner: learning.WithMemoryCapture(d.Runner, d.Memory), Store: d.Store, Limiter: d.Limiter,
			Cfg: d.Config, Index: d.BuildIndex, BotID: d.BotUserID,
			BotUsername: strings.TrimPrefix(d.BotUsername, "@"), Log: d.Logger, Now: d.Now,
		},
		rx:     newReactor(d.Reactions),
		onSkip: d.OnSkip, onRoute: d.OnRoute, cfgTTL: d.RepoConfigTTL, mrFlags: d.MRFlags,
	}
	if r.cfgTTL == 0 {
		r.cfgTTL = DefaultRepoConfigTTL
	}
	if r.env.Cfg == nil {
		c := config.Defaults()
		r.env.Cfg = &c
	}
	if r.env.Log == nil {
		r.env.Log = slog.New(slog.DiscardHandler)
	}
	if r.env.Now == nil {
		r.env.Now = time.Now
	}
	if r.env.Index == nil {
		r.env.Index = func([]core.FileDiff) env.DiffIndex { return env.EmptyIndex{} }
	}
	r.env.RepoConfig = repoConfigs{r}
	r.pub = publish.New(r.env)
	r.task = task.New(r.env, r.pub, d.Tasks)
	r.lint = linters.New(r.env, r.pub, r.task, d.Lint)
	r.learning = learning.New(r.env, learnReactions{r}, d.Learnings, d.Outcomes, d.Memory, d.ScratchDir)
	r.learning.Chat = r.threadQuestion
	r.learning.Dismiss = r.dismissThread
	r.crossrepo = crossrepo.New(r.env, r.task, d.ContextRepos, d.SystemMap)
	r.compat = compatcheck.New(r.env, r.pub, d.Compat)
	r.migrations = migcheck.New(r.env)
	r.verify = verify.New(r.env, r.learning, d.Verify)
	r.labels = labels.New(r.env, d.Labels, d.ServiceLabels, d.LabelStore, d.OnLabel)
	r.descsum = descsum.New(r.env, d.Description, d.ServiceDescription)
	r.mcp = mcp.New(r.env, r.pub, r.task, r.lint, r.migrations, r.crossrepo, d.MCP)
	scrub := d.Scrub
	if scrub == nil {
		scrub = diag.ScrubFunc(nil)
	}
	r.pipeline = pipeline.New(r.env, r.pub, r.lint, pipelineHost{r}, scrub, d.CI, d.CIMain)
	return r
}

// Handle is a core.Handler.
func (r *Reviewer) Handle(ctx context.Context, job core.Job) (err error) {
	ctx = jobctx.AttributeJob(ctx, job) // token accounting, see jobctx
	ctx = withGitLabMemo(ctx)           // repeated MR reads of this job (glmemo.go)
	// Award emoji on the job's note: 👀 … ✅/❌/⏳/🚫 (reactions.go).
	ctx, rs := r.reactBegin(ctx, job)
	defer func() {
		if p := recover(); p != nil {
			r.reactEnd(ctx, rs, fmt.Errorf("panic: %v", p))
			panic(p)
		}
		r.reactEnd(ctx, rs, err)
	}()
	switch job.Kind {
	case core.JobAutoReview, core.JobReview, core.JobFullReview, core.JobForceReview:
		return r.handleReview(ctx, job)
	case core.JobChat:
		return r.handleChat(ctx, job)
	case core.JobHelp:
		return r.handleHelp(ctx, job)
	case core.JobRefreshTitle:
		return r.task.HandleRefreshTitle(ctx, job)
	case core.JobRefreshStatus:
		return r.pipeline.HandleRefreshStatus(ctx, job) // pipeline/tbd.go: no Claude, no limits
	case core.JobThreadReply:
		return r.learning.HandleThreadReply(ctx, job)
	case core.JobRules:
		return r.learning.HandleRules(ctx, job)
	case core.JobPause, core.JobResume, core.JobIgnore, core.JobResolveAll, core.JobConfig:
		return r.handleMRCommand(ctx, job) // commands.go: no Claude, no limits
	case core.JobMemory:
		return r.learning.HandleMemory(ctx, job)
	case core.JobReact:
		return r.handleReact(ctx, job) // reactions.go: no Claude, no reply, no limits
	case core.JobCheck:
		return r.handleCheck(ctx, job) // check.go: re-check open findings
	case core.JobPipeline:
		return r.pipeline.HandlePipeline(ctx, job) // pipeline/ci.go: failed pipelines, no reply
	default:
		return fmt.Errorf("review: unknown job kind %q", job.Kind)
	}
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// effectiveConfig loads .reviewbot.yaml from the target branch (cached, see
// repoconfig.go) and merges it with the service config. A *repoConfigError
// means the file is invalid, or missing while review.require_repo_config
// is on (notOptedIn); answer commands with denyRepoConfig.
func (r *Reviewer) effectiveConfig(ctx context.Context, projectID int64, mr *core.MergeRequest) (config.Effective, error) {
	repo, err := r.targetRepoConfig(ctx, projectID, mr)
	if err != nil {
		return config.Effective{}, err
	}
	eff := config.Merge(r.env.Cfg, repo)
	r.rememberReactions(projectID, mr.IID, eff) // reactions.go
	return eff, nil
}

// ensureReviewer adds the bot to the MR's reviewers (service token only).
func (r *Reviewer) ensureReviewer(ctx context.Context, job core.Job) {
	if r.env.Mod == nil || !r.env.Cfg.Review.AssignReviewer || r.env.BotID == 0 {
		return
	}
	added, err := r.env.Mod.AddReviewer(ctx, job.ProjectID, job.MRIID, r.env.BotID)
	if err != nil {
		r.env.Log.Warn("review: assign bot as reviewer", "mr", job.MRKey(), "err", err)
		return
	}
	if added {
		r.env.Log.Info("review: bot assigned as reviewer", "mr", job.MRKey())
	}
}
