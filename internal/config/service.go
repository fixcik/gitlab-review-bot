package config

import (
	"time"
)

type Service struct {
	Listen  string  `yaml:"listen"`   // ":8080"
	DataDir string  `yaml:"data_dir"` // clones, worktrees, sqlite db
	GitLab  GitLab  `yaml:"gitlab"`
	Claude  Claude  `yaml:"claude"`
	Sandbox Sandbox `yaml:"sandbox"`
	Review  Review  `yaml:"review"`
	Limits  Limits  `yaml:"limits"`
	Jira    Jira    `yaml:"jira"`
	Usage   Usage   `yaml:"usage"` // token consumption page, see usage.go
	Diag    Diag    `yaml:"diag"`  // diagnostics of recent runs, see diag.go
	// Metrics: Prometheus /metrics, see metrics.go.
	Metrics Metrics `yaml:"metrics"`
	// Tracing: OpenTelemetry traces over OTLP/HTTP, see tracing.go.
	Tracing Tracing `yaml:"tracing"`
	// Queue: durability and graceful shutdown of the job queue, see queue.go.
	Queue Queue `yaml:"queue"`
}

type Claude struct {
	// Model runs reviews, thread answers and compat verification. Default
	// "sonnet"; empty = CLI default.
	Model   string   `yaml:"model"`
	Timeout Duration `yaml:"timeout"` // per run
	// AdvisorModel enables Claude Code's server-side advisor tool for those
	// runs: Model consults it on hard calls (blocking findings, breaking
	// changes). Must be at least as capable as Model. Default "opus"; empty
	// = off. If the account or model does not support the advisor, the bot
	// logs it once and runs without it.
	AdvisorModel string `yaml:"advisor_model"`
	// SearchModel runs the read-only "code-search" subagent (Read, Grep,
	// Glob) that Model may delegate broad searches to. Default "haiku";
	// empty = no subagent.
	SearchModel string `yaml:"search_model"`
	// DeepReviewModel runs the read-only "deep-review" subagent (Read, Grep,
	// Glob) that Model asks once for a second opinion on a non-trivial MR
	// (normal review route only). Default "opus"; empty = no subagent.
	DeepReviewModel string `yaml:"deep_review_model"`
	// LightModel is used for cheap, simple runs (extracting team rules from
	// thread replies). Default "haiku" (the CLI accepts aliases); empty = CLI
	// default. The review and chat runs use Model.
	LightModel string `yaml:"light_model"`
	// Auth is taken from env: CLAUDE_CODE_OAUTH_TOKEN (subscription,
	// from `claude setup-token`) or ANTHROPIC_API_KEY.
}

type Sandbox struct {
	// Mode "docker" (default) runs claude inside a container; "local" runs
	// the claude binary on the host with no isolation (prototyping only).
	Mode string `yaml:"mode"`
	// Image with the claude CLI and the bot's checkers, see
	// Dockerfile.sandbox.
	Image string `yaml:"image"`
	// Docker network the review container joins. It must be an internal
	// network whose only way out is the egress proxy.
	Network string `yaml:"network"`
	// EgressProxy as seen from inside the sandbox network.
	EgressProxy string `yaml:"egress_proxy"` // http://egress:3128
	// Hosts the egress proxy lets through.
	EgressAllow []string `yaml:"egress_allow"`
	CPUs        string   `yaml:"cpus"`   // docker --cpus
	Memory      string   `yaml:"memory"` // docker --memory
}

type Review struct {
	// RequireRepoConfig: only repositories with a .reviewbot.yaml on the
	// MR's target branch are reviewed (opt-in; an empty file means the
	// defaults). Default true; false reviews every project of gitlab.groups.
	// Context repositories and the system map are not affected.
	RequireRepoConfig bool `yaml:"require_repo_config"`
	// BotMention is DEPRECATED and ignored: the mention is always "@" + the
	// username of the GITLAB_TOKEN account, so it can't drift from the real
	// account. Kept only to warn about old configs.
	BotMention    string   `yaml:"bot_mention"`
	Debounce      Duration `yaml:"debounce"`
	MaxDiffBytes  int      `yaml:"max_diff_bytes"`
	DefaultLang   string   `yaml:"default_language"`
	DefaultIgnore []string `yaml:"default_ignore"`
	MaxFindings   int      `yaml:"max_findings"`
	// Memory: per-project operational memory (see memory.go).
	Memory Memory `yaml:"memory"`
	// Skills: team skills directory (see skills.go).
	Skills Skills `yaml:"skills"`
	// TBD: trunk-based status block of the summary (see tbd.go).
	TBD TBD `yaml:"tbd"`
	// Coverage: diff coverage from CI artifacts (see coverage.go).
	Coverage Coverage `yaml:"coverage"`
	// Migrations: the migration safety check (see migrations.go).
	Migrations Migrations `yaml:"migrations"`
	// BlockMinSeverity: findings at or above it stay open threads, which
	// block the merge when the project requires all threads to be resolved.
	// Lower findings are resolved right away by the service token (visible,
	// not blocking). critical | major | minor | nit; default critical.
	BlockMinSeverity string `yaml:"block_min_severity"`
	// AssignReviewer adds the bot to the MR's reviewers (via the service
	// token) when it reviews. Default true; no-op without a service token.
	AssignReviewer bool `yaml:"assign_reviewer"`
	// Learn configures learning team rules from replies in bot threads.
	Learn Learn `yaml:"learn"`
	// Context: read-only snapshots of the other repositories of
	// gitlab.groups for Claude (see context.go).
	Context ReviewContext `yaml:"context"`
	// CompatCheck enables the backward-compatibility check (deterministic
	// spec diff tools + Claude's semantic check + verification). Default
	// true; a repository can still turn it off with compat.enabled: false.
	CompatCheck bool `yaml:"compat_check"`
	// CompatTimeout bounds each deterministic compat tool run.
	CompatTimeout Duration `yaml:"compat_timeout"`
	// Diagram asks Claude for a Mermaid diagram of the change for the
	// summary (default true; a repo can turn it off with diagram: false).
	Diagram bool `yaml:"diagram"`
	// DescriptionSummary keeps a short summary of the changes in the MR's
	// own description, between markers (review/descsum; default false: the
	// bot would write into other people's descriptions; a repo can turn it
	// off with description_summary: false).
	DescriptionSummary bool `yaml:"description_summary"`
	// PathInstructions apply to every repository (see paths.go).
	PathInstructions []PathInstruction `yaml:"path_instructions"`
	// Lint: deterministic linters and security scanners (see lint.go).
	Lint ReviewLint `yaml:"lint"`
	// Todo: TODO/FIXME comments must reference a task (see todo.go).
	Todo ReviewTodo `yaml:"todo"`
	// Sysmap: the system map of contracts between repositories (see
	// sysmap.go).
	Sysmap ReviewSysmap `yaml:"sysmap"`
	// MCP: the bot's MCP server for review tools (see mcp.go).
	MCP MCP `yaml:"mcp"`
	// Problems: what the bot could not check (see problems.go).
	Problems Problems `yaml:"problems"`
	// Reactions: award emoji on notes addressed to the bot and on MRs
	// (see reactions.go).
	Reactions Reactions `yaml:"reactions"`
	// Labels: automatic MR labels (see labels.go).
	Labels Labels `yaml:"labels"`
	// Verify: adversarial verification of findings before publishing (see
	// verify.go).
	Verify Verify `yaml:"verify"`
	// MaxMRLines: changed lines (added + removed, ignored files excluded) of
	// the reviewed range above which Claude does not run (see bigmr.go);
	// 0 = no limit.
	MaxMRLines int `yaml:"max_mr_lines"`
	// BigMR: what happens to an MR over MaxMRLines (see bigmr.go).
	BigMR BigMR `yaml:"big_mr"`
	// CI: digest and explanation of failed pipelines (see ci.go).
	CI CI `yaml:"ci"`
	// ParallelAnalyzers runs the review's independent analyzers (base export,
	// compat, linters, Jira, context, coverage, system map, migrations)
	// concurrently; false = one after the other (internal/review/analyzers.go).
	ParallelAnalyzers bool `yaml:"parallel_analyzers"`
	// CheckoutCache: shared read-only checkouts kept after use (see perf.go).
	CheckoutCache CheckoutCache `yaml:"checkout_cache"`
	// Routing: low-risk reviews go to a light model (see routing.go).
	Routing Routing `yaml:"routing"`
	// SkipTrivialIncrements: an automatic incremental review whose new
	// commits change only whitespace, comments or documentation runs no
	// Claude (deterministic checks only). Default true.
	SkipTrivialIncrements bool `yaml:"skip_trivial_increments"`
}

type Limits struct {
	GlobalRunsPerHour int      `yaml:"global_runs_per_hour"`
	UserRerunsPerHour int      `yaml:"user_reruns_per_hour"`
	UserChatPerHour   int      `yaml:"user_chat_per_hour"`
	MRRerunCooldown   Duration `yaml:"mr_rerun_cooldown"`
	MaxConcurrent     int      `yaml:"max_concurrent"`
	ExemptUsers       []string `yaml:"exempt_users"`
}

// Defaults returns a config with every field set to its default.
func Defaults() Service {
	return Service{
		Listen:  ":8080",
		DataDir: "./data",
		Claude: Claude{Timeout: Duration(15 * time.Minute), LightModel: "haiku",
			Model: "sonnet", AdvisorModel: "opus", SearchModel: "haiku", DeepReviewModel: "opus"},
		Sandbox: Sandbox{
			Mode:        "docker",
			Image:       "reviewbot-sandbox:latest",
			Network:     "reviewbot-sandbox",
			EgressProxy: "http://egress:3128",
			EgressAllow: []string{"api.anthropic.com", "statsig.anthropic.com", "claude.ai", "console.anthropic.com"},
			CPUs:        "2",
			Memory:      "4g",
		},
		Review: Review{
			RequireRepoConfig: true,
			Debounce:          Duration(90 * time.Second),
			MaxDiffBytes:      400_000,
			DefaultLang:       "ru",
			DefaultIgnore:     []string{"**/*.lock", "**/go.sum", "**/package-lock.json", "**/*.min.js", "vendor/**", "node_modules/**"},
			MaxFindings:       30,

			BlockMinSeverity: "critical",
			AssignReviewer:   true,
			Learn:            defaultLearn(),

			Context:       defaultReviewContext(),
			TBD:           defaultTBD(),
			Coverage:      defaultCoverage(),
			Migrations:    defaultMigrations(),
			CompatCheck:   true,
			CompatTimeout: Duration(3 * time.Minute),
			Diagram:       true,
			Lint:          defaultReviewLint(),
			Todo:          defaultReviewTodo(),
			Sysmap:        defaultReviewSysmap(),
			MCP:           defaultMCP(),
			Problems:      defaultProblems(),
			Reactions:     defaultReactions(),
			Labels:        defaultLabels(),
			Verify:        defaultVerify(),
			MaxMRLines:    DefaultMaxMRLines,
			BigMR:         defaultBigMR(),
			CI:            defaultCI(),

			ParallelAnalyzers: true,
			CheckoutCache:     defaultCheckoutCache(),
			Routing:           defaultRouting(),

			SkipTrivialIncrements: true,
		},
		Limits: Limits{
			GlobalRunsPerHour: 15,
			UserRerunsPerHour: 5,
			UserChatPerHour:   20,
			MRRerunCooldown:   Duration(10 * time.Minute),
			MaxConcurrent:     2,
		},
		GitLab: GitLab{
			SyncInterval:  Duration(time.Hour),
			HookSSLVerify: true,
			Events:        EventsWebhook,
			PollInterval:  Duration(5 * time.Second),
		},
		Jira:    defaultJira(),
		Tracing: defaultTracing(),
		Queue:   defaultQueue(),
	}
}

// MaxRerunCooldown caps limits.mr_rerun_cooldown (service and repository):
// the limiter keeps usage records for 24 hours (internal/limits), so a
// longer cooldown would silently stop working after a day.
const MaxRerunCooldown = 24 * time.Hour
