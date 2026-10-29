package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

type GitLab struct {
	URL string `yaml:"url"` // https://gitlab.example.com
	// Secrets come from env by default; the yaml fields are a fallback.
	Token         string `yaml:"token"`          // env GITLAB_TOKEN
	WebhookSecret string `yaml:"webhook_secret"` // env GITLAB_WEBHOOK_SECRET
	// ServiceToken (env GITLAB_SERVICE_TOKEN; GITLAB_HOOKS_TOKEN is accepted
	// too) belongs to a separate Maintainer account. It is used ONLY to sync
	// webhooks, add the bot as MR reviewer and resolve threads; its client
	// may write nothing else. The bot's own Token should belong to a
	// Reporter, so it cannot push, merge or change settings.
	ServiceToken string `yaml:"service_token"`
	// Allow self-signed certs on an internal GitLab.
	CAFile string `yaml:"ca_file"`

	// Groups are full group paths ("backend", "platform/services") whose
	// projects (subgroups included) the bot reviews. Webhook events from other
	// projects are ignored. Empty = accept every project that has a webhook.
	Groups []string `yaml:"groups"`
	// Exclude are doublestar globs of project paths ("backend/legacy-*",
	// "frontend/sandbox/**") the bot does not review: events are ignored,
	// no hooks are synced. They stay available as context repositories
	// unless review.context.exclude lists them too.
	Exclude []string `yaml:"exclude"`
	// MainBranch is the "main" (deployed) branch of the group repositories
	// used for context snapshots, the system map and "not yet in main"
	// checks. Empty = each project's GitLab default branch. A branch that a
	// repository does not have falls back to its default branch.
	MainBranch string `yaml:"main_branch"`
	// MainBranches overrides MainBranch per project: doublestar glob of the
	// project path -> branch ("legacy/**": master). The most specific
	// (longest) matching glob wins.
	MainBranches map[string]string `yaml:"main_branches"`
	// WebhookURL is the bot's /webhook endpoint as GitLab sees it. Required
	// for SyncHooks.
	WebhookURL string `yaml:"webhook_url"`
	// SyncHooks makes the bot create/fix its project webhook in every project
	// of Groups (GitLab Free/CE has no group webhooks), using ServiceToken,
	// whose user needs Maintainer in those projects.
	SyncHooks bool `yaml:"sync_hooks"`
	// SyncInterval is how often hooks are re-synced (new projects).
	SyncInterval Duration `yaml:"sync_interval"`
	// HookSSLVerify is enable_ssl_verification on the managed hooks.
	HookSSLVerify bool `yaml:"hook_ssl_verify"`

	// Events selects how the bot learns about MR changes and comments:
	// "webhook" (default; GitLab must reach /webhook), "poll" (the bot asks
	// the GitLab API every PollInterval; no inbound connection, no webhook
	// secret) or "both" (webhook plus polling as a safety net; duplicates
	// are dropped).
	Events string `yaml:"events"`
	// PollInterval is the polling period for Events "poll" and "both".
	PollInterval Duration `yaml:"poll_interval"`
}

// Event source modes (GitLab.Events).
const (
	EventsWebhook = "webhook"
	EventsPoll    = "poll"
	EventsBoth    = "both"
)

// MinPollInterval is the shortest accepted gitlab.poll_interval.
const MinPollInterval = 5 * time.Second

// UsesWebhook reports whether the /webhook endpoint is served.
func (g GitLab) UsesWebhook() bool { return g.Events != EventsPoll }

// UsesPolling reports whether the bot polls the GitLab API for events.
func (g GitLab) UsesPolling() bool { return g.Events == EventsPoll || g.Events == EventsBoth }

// NormalizeGroups trims spaces and slashes from group paths and drops empty
// and duplicate (case-insensitive) entries, keeping the first spelling.
func NormalizeGroups(groups []string) []string {
	var out []string
	seen := make(map[string]bool, len(groups))
	for _, g := range groups {
		g = strings.Trim(strings.TrimSpace(g), "/")
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		k := strings.ToLower(g)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, g)
	}
	return out
}

// Excluded reports whether a project path matches gitlab.exclude
// (case-insensitive doublestar globs).
func (g GitLab) Excluded(projectPath string) bool { return matchGlobs(g.Exclude, projectPath) }

// branchNameRe is a conservative git branch name check (no spaces, no
// "..", no leading "-", no control or special characters).
var branchNameRe = regexp.MustCompile(`^[A-Za-z0-9._/][A-Za-z0-9._/+-]*$`)

func validBranch(b string) bool {
	return b != "" && branchNameRe.MatchString(b) && !strings.Contains(b, "..") && !strings.HasSuffix(b, "/") &&
		!strings.HasSuffix(b, ".lock") && !strings.Contains(b, "//")
}

func (g GitLab) validateMainBranches() []string {
	var errs []string
	if b := strings.TrimSpace(g.MainBranch); b != "" && !validBranch(b) {
		errs = append(errs, fmt.Sprintf("gitlab.main_branch: invalid branch name %q", b))
	}
	for glob, b := range g.MainBranches {
		if !doublestar.ValidatePattern(strings.Trim(strings.TrimSpace(glob), "/")) {
			errs = append(errs, fmt.Sprintf("gitlab.main_branches: invalid glob %q", glob))
		}
		if !validBranch(strings.TrimSpace(b)) {
			errs = append(errs, fmt.Sprintf("gitlab.main_branches[%q]: invalid branch name %q", glob, b))
		}
	}
	sort.Strings(errs)
	return errs
}

// MainBranchFor returns the main (deployed) branch of a project: the most
// specific matching gitlab.main_branches glob, else gitlab.main_branch,
// else defaultBranch (the project's GitLab default branch).
func (g GitLab) MainBranchFor(projectPath, defaultBranch string) string {
	p := strings.ToLower(strings.Trim(strings.TrimSpace(projectPath), "/"))
	// Equally long globs: the lexicographically smaller glob wins, so the
	// result does not depend on map iteration order.
	best, bestGlob, bestLen := "", "", -1
	for glob, b := range g.MainBranches {
		gl := strings.ToLower(strings.Trim(strings.TrimSpace(glob), "/"))
		if ok, err := doublestar.Match(gl, p); err == nil && ok {
			if len(gl) > bestLen || (len(gl) == bestLen && gl < bestGlob) {
				best, bestGlob, bestLen = strings.TrimSpace(b), gl, len(gl)
			}
		}
	}
	if best != "" {
		return best
	}
	if b := strings.TrimSpace(g.MainBranch); b != "" {
		return b
	}
	return defaultBranch
}
