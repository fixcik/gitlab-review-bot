package config

type Repo struct {
	// Enabled=false turns the bot off for the repo (commands still answer).
	Enabled *bool `yaml:"enabled"`
	// AutoReview=false: no automatic reviews; commands still work.
	AutoReview *bool `yaml:"auto_review"`
	// Language of the review ("ru", "en"); default review.default_language.
	Language string `yaml:"language"`
	// Ignore: doublestar globs of files not reviewed (added to the service
	// review.default_ignore).
	Ignore []string `yaml:"ignore"`
	// Instructions: free text for the reviewer about this repository.
	Instructions string `yaml:"instructions"`
	// MaxFiles: at most this many files are reviewed (default 200).
	MaxFiles int `yaml:"max_files"`
	// Limits can only make the service limits stricter.
	Limits *RepoLimits `yaml:"limits"`
	// Jira: per-repository Jira settings.
	Jira *RepoJira `yaml:"jira"`
	// Memory: enabled=false neither reads nor writes the project memory.
	Memory *RepoMemory `yaml:"memory"`
	// Learn: enabled=false neither learns from this repo's threads nor
	// applies learned rules in its reviews.
	Learn *RepoLearn `yaml:"learn"`
	// Context controls context repositories for this repo (see context.go).
	Context *RepoContext `yaml:"context"`
	Compat  *RepoCompat  `yaml:"compat"`
	// Diagram=false drops the Mermaid diagram from this repo's summaries.
	Diagram *bool `yaml:"diagram"`
	// DescriptionSummary=false keeps the bot out of this repo's MR
	// descriptions (it cannot turn review.description_summary on).
	DescriptionSummary *bool `yaml:"description_summary"`
	// PathInstructions: extra instructions for files matching a glob
	// (see paths.go).
	PathInstructions []PathInstruction `yaml:"path_instructions"`
	// Lint: linters and scanners for this repo (see lint.go).
	Lint *RepoLint `yaml:"lint"`
	// Todo: the TODO-without-task check for this repo (see todo.go).
	Todo *RepoTodo `yaml:"todo"`
	// Skills: which team skills this repo's reviews get (see skills.go).
	Skills *RepoSkills `yaml:"skills"`
	// TBD: thresholds of the trunk-based status block (see tbd.go).
	TBD *RepoTBD `yaml:"tbd"`
	// Coverage: which CI job artifact holds the coverage report.
	Coverage *RepoCoverage `yaml:"coverage"`
	// Migrations: the migration safety check for this repo (see migrations.go).
	Migrations *RepoMigrations `yaml:"migrations"`
	// Sysmap: files of this repository the system map never scans.
	Sysmap *RepoSysmap `yaml:"sysmap"`
	// MCP: enabled=false turns the review tools of the MCP server off for
	// this repository (it cannot turn them on).
	MCP *RepoMCP `yaml:"mcp"`
	// Problems: summary=false hides the "could not check" block (and its
	// mentions) in this repository's summaries (see problems.go).
	Problems *RepoProblems `yaml:"problems"`
	// Reactions: enabled=false turns the bot's award emoji off for this
	// repository (it cannot turn them on or change them; see reactions.go).
	Reactions *RepoReactions `yaml:"reactions"`
	// Labels: automatic MR labels for this repository (see labels.go).
	Labels *RepoLabels `yaml:"labels"`
	// Verify: enabled=false turns the verification of findings off for this
	// repository (it cannot turn it on; see verify.go).
	Verify *RepoVerify `yaml:"verify"`
	// MaxMRLines: a stricter (lower) review.max_mr_lines for this
	// repository; 0 = the service value (see bigmr.go).
	MaxMRLines int `yaml:"max_mr_lines"`
	// BigMR: block=true makes a too-big MR merge-blocking here.
	BigMR *RepoBigMR `yaml:"big_mr"`
	// CI: enabled=false turns the CI failure digest off for this
	// repository; ignore_jobs adds job globs (see ci.go).
	CI *RepoCI `yaml:"ci"`
}

// RepoCompat configures the backward-compatibility check for one repository.
type RepoCompat struct {
	// Enabled=false turns the check off for the repository (default true).
	Enabled *bool `yaml:"enabled"`
	// OpenAPI, Proto and GraphQL are extra path globs (doublestar syntax) of
	// spec files, on top of the built-in discovery.
	OpenAPI []string `yaml:"openapi"`
	Proto   []string `yaml:"proto"`
	GraphQL []string `yaml:"graphql"`
	// compat.commands (repository-defined checker commands) was removed:
	// see removedRepoKeys.
}

// RepoJira overrides Jira settings for one repository.
type RepoJira struct {
	// ProjectKeys replaces jira.project_keys of the service for this repo,
	// e.g. ["PROJ"]. Empty keeps the service value.
	ProjectKeys []string `yaml:"project_keys"`
	// TitlePrompt / TitleLanguage replace jira.title_prompt /
	// jira.title_language of the service for this repository (empty =
	// the service value).
	TitlePrompt   string `yaml:"title_prompt"`
	TitleLanguage string `yaml:"title_language"`
}

// RepoLimits can only make the service limits stricter.
type RepoLimits struct {
	UserRerunsPerHour int      `yaml:"user_reruns_per_hour"`
	UserChatPerHour   int      `yaml:"user_chat_per_hour"`
	MRRerunCooldown   Duration `yaml:"mr_rerun_cooldown"`
}

// ParseRepo parses .reviewbot.yaml strictly: unknown keys, wrong types and
// invalid values are errors. Empty input yields an empty config. The error
// is RepoErrors with every problem found (see ValidateRepo, RepoIssuesOf).
func ParseRepo(b []byte) (*Repo, error) {
	r, issues := ValidateRepo(b)
	if len(issues) > 0 {
		return nil, issues
	}
	return r, nil
}
