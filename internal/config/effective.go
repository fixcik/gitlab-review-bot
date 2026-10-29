package config

import (
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Effective is the merged view the reviewer works with.
type Effective struct {
	Enabled      bool
	AutoReview   bool
	Language     string
	Ignore       []string
	Instructions string
	MaxFiles     int
	Limits       Limits
	// JiraProjectKeys are the issue key prefixes for this repo: the repo's
	// jira.project_keys when set, otherwise the service's.
	JiraProjectKeys []string
	// TitlePrompt / TitleLanguage: the MR title format and language
	// (jira.title_*; the repo's jira section overrides; title.go).
	TitlePrompt   string
	TitleLanguage string
	// MemoryEnabled: review.memory.enabled AND the repo's memory.enabled.
	MemoryEnabled bool
	// LearnEnabled: learn rules from this repo and apply them here
	// (review.learn.enabled and the repo's learn.enabled).
	LearnEnabled bool
	// Context is the repo's `context` section (nil when absent).
	Context *RepoContext
	// Compat is the backward-compatibility check configuration.
	Compat Compat
	// Diagram: review.diagram of the service AND diagram of the repo.
	Diagram bool
	// DescriptionSummary: review.description_summary of the service AND
	// description_summary of the repo.
	DescriptionSummary bool
	// PathInstructions: service entries, then the repo's (see paths.go).
	PathInstructions []PathInstruction
	// Lint is the linters and scanners configuration (see lint.go).
	Lint Lint
	// Todo: the TODO-without-task check (service + repo, see todo.go).
	Todo Todo
	// SkillsEnabled: review.skills.dir is set AND the repo's skills.enabled.
	SkillsEnabled bool
	// SkillsDisable: skill names the repo turned off (lowercase).
	SkillsDisable []string
	// TBD: the trunk-based status block settings (service + repo).
	TBD TBD
	// Coverage: diff coverage from CI artifacts (service + repo).
	Coverage EffCoverage
	// Migrations: the migration safety check (service + repo).
	Migrations Migrations
	// SysmapExclude: the repo's sysmap.exclude file globs (system map).
	SysmapExclude []string
	// MCP: review.mcp.enabled of the service AND mcp.enabled of the repo.
	MCP bool
	// ProblemsSummary: review.problems.enabled of the service AND
	// problems.summary of the repo (the summary block and its mentions).
	ProblemsSummary bool
	// Reactions: review.reactions of the service, with Enabled false when
	// the repo's reactions.enabled is false.
	Reactions Reactions
	// Labels: automatic MR labels (service + repo, see labels.go).
	Labels EffLabels
	// Verify: review.verify.enabled of the service AND verify.enabled of
	// the repo (verification of findings; breaking-change claims are
	// verified regardless).
	Verify bool
	// BigMR: the size limit of the review (service + stricter repo, see
	// bigmr.go).
	BigMR EffBigMR
	// CI: the CI failure digest (service + repo, see ci.go).
	CI EffCI
	// Routing: model routing by MR risk (service + safer repo, see
	// routing.go).
	Routing EffRouting
}

// Compat is the effective backward-compatibility check configuration.
type Compat struct {
	// Enabled: review.compat_check of the service AND compat.enabled of the
	// repository (default true).
	Enabled bool
	OpenAPI []string
	Proto   []string
	GraphQL []string
}

// Merge combines service defaults with a repo config. repo may be nil.
func Merge(svc *Service, repo *Repo) Effective {
	e := Effective{
		Enabled:    true,
		AutoReview: true,
		Language:   svc.Review.DefaultLang,
		Ignore:     append([]string{}, svc.Review.DefaultIgnore...),
		MaxFiles:   200,
		Limits:     svc.Limits,

		JiraProjectKeys: append([]string{}, svc.Jira.ProjectKeys...),
		TitlePrompt:     svc.Jira.TitlePrompt, TitleLanguage: titleLanguage(svc.Jira.TitleLanguage),
		Compat: Compat{Enabled: svc.Review.CompatCheck},
	}
	e.LearnEnabled = svc.Review.Learn.Enabled && repo.learnEnabled()
	e.Diagram = svc.Review.Diagram && repo.diagramEnabled()
	e.DescriptionSummary = svc.Review.DescriptionSummary && (repo == nil || defaultTrue(repo.DescriptionSummary))
	e.PathInstructions = mergePathInstructions(svc, repo)
	e.MemoryEnabled = svc.Review.Memory.On() && repo.RepoMemoryEnabled()
	e.SkillsEnabled, e.SkillsDisable = svc.Review.Skills.On() && repo.skillsEnabled(), repo.skillsDisable()
	e.TBD = mergeTBD(svc.Review.TBD, repo)
	e.Coverage = mergeCoverage(svc.Review.Coverage, repo)
	e.Migrations = mergeMigrations(svc.Review.Migrations, repo)
	e.Lint = mergeLint(svc, repo)
	e.Todo = mergeTodo(svc.Review.Todo, repo)
	e.MCP = svc.Review.MCP.Enabled && repo.mcpEnabled()
	e.ProblemsSummary = svc.Review.Problems.Enabled && repo.problemsSummary()
	e.Reactions = svc.Review.Reactions
	e.Reactions.IgnoreUsers = append([]string(nil), svc.Review.Reactions.IgnoreUsers...)
	e.Reactions.Enabled = svc.Review.Reactions.Enabled && repo.reactionsEnabled()
	e.Labels = mergeLabels(svc.Review.Labels, repo, svc.Review.BigMR.OverrideLabel)
	e.Verify = svc.Review.Verify.Enabled && repo.verifyEnabled()
	e.BigMR = mergeBigMR(svc.Review, repo)
	e.CI = mergeCI(svc.Review.CI, repo)
	e.Routing = mergeRouting(svc, repo)
	if repo == nil {
		return e
	}
	if c := repo.Compat; c != nil {
		e.Compat.Enabled = e.Compat.Enabled && defaultTrue(c.Enabled)
		e.Compat.OpenAPI = append([]string{}, c.OpenAPI...)
		e.Compat.Proto = append([]string{}, c.Proto...)
		e.Compat.GraphQL = append([]string{}, c.GraphQL...)
	}
	if repo.Enabled != nil {
		e.Enabled = *repo.Enabled
	}
	if repo.AutoReview != nil {
		e.AutoReview = *repo.AutoReview
	}
	if repo.Language != "" {
		e.Language = repo.Language
	}
	e.Ignore = append(e.Ignore, repo.Ignore...)
	e.Instructions = repo.Instructions
	if repo.MaxFiles > 0 {
		e.MaxFiles = repo.MaxFiles
	}
	if repo.Jira != nil && len(repo.Jira.ProjectKeys) > 0 {
		e.JiraProjectKeys = append([]string{}, repo.Jira.ProjectKeys...)
	}
	if repo.Jira != nil && strings.TrimSpace(repo.Jira.TitlePrompt) != "" {
		e.TitlePrompt = repo.Jira.TitlePrompt
	}
	if repo.Jira != nil && strings.TrimSpace(repo.Jira.TitleLanguage) != "" {
		e.TitleLanguage = strings.TrimSpace(repo.Jira.TitleLanguage)
	}
	e.Context = repo.Context
	if repo.Sysmap != nil {
		e.SysmapExclude = append([]string{}, repo.Sysmap.Exclude...)
	}
	if l := repo.Limits; l != nil {
		e.Limits.UserRerunsPerHour = stricter(e.Limits.UserRerunsPerHour, l.UserRerunsPerHour)
		e.Limits.UserChatPerHour = stricter(e.Limits.UserChatPerHour, l.UserChatPerHour)
		// Longer is stricter; a service cooldown of 0 (none) is tightened by
		// any positive repo value.
		if l.MRRerunCooldown > e.Limits.MRRerunCooldown {
			e.Limits.MRRerunCooldown = min(l.MRRerunCooldown, Duration(MaxRerunCooldown))
		}
	}
	return e
}

// UserLimits converts the effective limits for the limiter.
func (e Effective) UserLimits() core.UserLimits {
	return core.UserLimits{
		RerunsPerHour:   e.Limits.UserRerunsPerHour,
		ChatPerHour:     e.Limits.UserChatPerHour,
		MRRerunCooldown: e.Limits.MRRerunCooldown.D(),
	}
}

// stricter picks the stricter of two per-hour limits where 0 means
// "unlimited" in the service and "not set" in the repo: a positive repo value
// applies when it is lower than the service limit or when the service has no
// limit at all.
func stricter(svc, repo int) int {
	if repo > 0 && (svc <= 0 || repo < svc) {
		return repo
	}
	return svc
}
