package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// ReviewContext configures read-only access for Claude to the other
// repositories of gitlab.groups (default branch snapshots), used to find
// consumers and producers of what an MR changes.
type ReviewContext struct {
	// Enabled turns the feature on. It is effective only when gitlab.groups
	// is not empty (see Service.ContextEnabled).
	Enabled bool `yaml:"enabled"`
	// RefreshInterval is how often the project list is re-read and each
	// repository's default branch is re-fetched.
	RefreshInterval Duration `yaml:"refresh_interval"`
	// MaxRepos caps the number of repositories given to one run.
	MaxRepos int `yaml:"max_repos"`
	// MaxRepoMB skips repositories larger than this (GitLab statistics when
	// available, otherwise the size of the exported snapshot).
	MaxRepoMB int `yaml:"max_repo_mb"`
	// Exclude are doublestar globs of project paths never given to Claude
	// as context (for every repository), case-insensitive.
	Exclude []string `yaml:"exclude"`
}

// Excluded reports whether a project path matches review.context.exclude.
func (c ReviewContext) Excluded(projectPath string) bool { return matchGlobs(c.Exclude, projectPath) }

func defaultReviewContext() ReviewContext {
	return ReviewContext{
		Enabled:         true,
		RefreshInterval: Duration(10 * time.Minute),
		MaxRepos:        50,
		MaxRepoMB:       500,
	}
}

// MaxRepoBytes is MaxRepoMB in bytes.
func (c ReviewContext) MaxRepoBytes() int64 { return int64(c.MaxRepoMB) << 20 }

func (c ReviewContext) validate() []string {
	if !c.Enabled {
		return nil
	}
	var errs []string
	if c.RefreshInterval.D() < time.Minute {
		errs = append(errs, "review.context.refresh_interval must be >= 1m")
	}
	if c.MaxRepos < 1 {
		errs = append(errs, "review.context.max_repos must be >= 1")
	}
	if c.MaxRepoMB < 1 {
		errs = append(errs, "review.context.max_repo_mb must be >= 1")
	}
	return errs
}

// ContextEnabled reports whether context repositories are on: enabled in
// review.context and at least one group configured.
func (c *Service) ContextEnabled() bool {
	return c.Review.Context.Enabled && len(NormalizeGroups(c.GitLab.Groups)) > 0
}

// RepoContext is the per-repository `context` section of .reviewbot.yaml.
type RepoContext struct {
	// Enabled=false gives this repository's reviews no context repositories.
	Enabled *bool `yaml:"enabled"`
	// Exclude are doublestar globs of project paths ("legacy/**",
	// "group/huge-monorepo"), matched case-insensitively.
	Exclude []string `yaml:"exclude"`
	// Related are repositories this one works with (its frontend, clients,
	// the services it calls). They are always given to Claude first, beyond
	// review.context.max_repos and regardless of Exclude, may lie outside
	// gitlab.groups (the bot account needs read access), and the prompt
	// marks them with the note.
	Related []RelatedRepo `yaml:"related"`
}

// RelatedRepo is one entry of context.related.
type RelatedRepo struct {
	// Repo is the full project path, e.g. "frontend/web".
	Repo string `yaml:"repo"`
	// Note says how the repositories are related ("фронтенд, вызывает наш
	// REST API из src/api").
	Note string `yaml:"note"`
}

// Limits of context.related.
const (
	MaxRelatedRepos   = 20
	MaxRelatedNoteLen = 500
)

// relatedPathRe: GitLab full paths (same rule as repo.ValidProjectPath,
// without importing it).
var relatedPathRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]*(/[A-Za-z0-9_][A-Za-z0-9_.\-]*)+$`)

func (c *RepoContext) normalize() error {
	if c == nil {
		return nil
	}
	ex, err := cleanGlobList(c.Exclude)
	if err != nil {
		return err
	}
	c.Exclude = ex
	if len(c.Related) > MaxRelatedRepos {
		return fmt.Errorf("context.related: at most %d entries", MaxRelatedRepos)
	}
	seen := map[string]bool{}
	rel := c.Related[:0:0]
	for _, r := range c.Related {
		r.Repo = strings.Trim(strings.TrimSpace(r.Repo), "/")
		r.Note = strings.Join(strings.Fields(r.Note), " ")
		if r.Repo == "" {
			return fmt.Errorf("context.related: empty repo")
		}
		if !relatedPathRe.MatchString(r.Repo) || strings.Contains(r.Repo, "..") || strings.HasSuffix(r.Repo, ".git") {
			return fmt.Errorf("context.related: %q is not a full project path like group/project", r.Repo)
		}
		if len([]rune(r.Note)) > MaxRelatedNoteLen {
			return fmt.Errorf("context.related: note for %s is longer than %d characters", r.Repo, MaxRelatedNoteLen)
		}
		if k := strings.ToLower(r.Repo); !seen[k] {
			seen[k] = true
			rel = append(rel, r)
		}
	}
	c.Related = rel
	return nil
}

// ContextRelated returns the repo's context.related entries (nil when none).
func (e Effective) ContextRelated() []RelatedRepo {
	if e.Context == nil {
		return nil
	}
	return e.Context.Related
}

// ContextEnabled reports whether the repository allows context repositories
// (default true). The service switch is Service.ContextEnabled.
func (e Effective) ContextEnabled() bool {
	return e.Context == nil || defaultTrue(e.Context.Enabled)
}

// ContextExcluded reports whether projectPath matches one of the repo's
// exclude globs (case-insensitive).
func (e Effective) ContextExcluded(projectPath string) bool {
	if e.Context == nil {
		return false
	}
	p := strings.ToLower(strings.Trim(projectPath, "/"))
	for _, g := range e.Context.Exclude {
		if ok, err := doublestar.Match(strings.ToLower(g), p); err == nil && ok {
			return true
		}
	}
	return false
}
