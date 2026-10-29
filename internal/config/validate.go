package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

func (c *Service) Validate() error {
	var errs []string
	if c.GitLab.URL == "" {
		errs = append(errs, "gitlab.url (or GITLAB_URL) is required")
	}
	if c.GitLab.Token == "" {
		errs = append(errs, "GITLAB_TOKEN is required")
	}
	switch c.GitLab.Events {
	case EventsWebhook, EventsPoll, EventsBoth:
	default:
		errs = append(errs, `gitlab.events must be "webhook", "poll" or "both"`)
	}
	if c.GitLab.UsesWebhook() && c.GitLab.WebhookSecret == "" {
		errs = append(errs, `GITLAB_WEBHOOK_SECRET is required (or set gitlab.events: "poll" to work without a webhook)`)
	}
	if c.GitLab.UsesPolling() {
		if c.GitLab.PollInterval.D() < MinPollInterval {
			errs = append(errs, "gitlab.poll_interval must be >= 5s")
		}
		// Without groups the bot would poll every MR its account can see,
		// public projects of the whole instance included.
		if len(NormalizeGroups(c.GitLab.Groups)) == 0 {
			errs = append(errs, `gitlab.events "poll"/"both" requires at least one entry in gitlab.groups`)
		}
	}
	if c.GitLab.Events == EventsPoll && c.GitLab.SyncHooks {
		errs = append(errs, `gitlab.sync_hooks makes no sense with gitlab.events: "poll" (no webhook is served)`)
	}
	errs = append(errs, validateGlobs("gitlab.exclude", c.GitLab.Exclude)...)
	errs = append(errs, c.GitLab.validateMainBranches()...)
	errs = append(errs, validateGlobs("review.context.exclude", c.Review.Context.Exclude)...)
	if c.Sandbox.Mode != "docker" && c.Sandbox.Mode != "local" {
		errs = append(errs, `sandbox.mode must be "docker" or "local"`)
	}
	if c.Limits.MaxConcurrent < 1 {
		errs = append(errs, "limits.max_concurrent must be >= 1")
	}
	if c.Limits.MRRerunCooldown < 0 || c.Limits.MRRerunCooldown.D() > MaxRerunCooldown {
		errs = append(errs, "limits.mr_rerun_cooldown must be between 0 and 24h")
	}
	if c.GitLab.WebhookURL != "" && !validHTTPURL(c.GitLab.WebhookURL) {
		errs = append(errs, "gitlab.webhook_url must be an http(s) URL")
	}
	if c.GitLab.SyncHooks {
		if c.GitLab.WebhookURL == "" {
			errs = append(errs, "gitlab.sync_hooks requires gitlab.webhook_url")
		}
		if c.GitLab.ServiceToken == "" {
			errs = append(errs, "gitlab.sync_hooks requires GITLAB_SERVICE_TOKEN (a separate Maintainer token; the bot token should be Reporter)")
		}
		if len(NormalizeGroups(c.GitLab.Groups)) == 0 {
			errs = append(errs, "gitlab.sync_hooks requires at least one entry in gitlab.groups")
		}
		if c.GitLab.SyncInterval.D() < time.Minute {
			errs = append(errs, "gitlab.sync_interval must be >= 1m")
		}
	}
	if m := c.Claude.DeepReviewModel; m != "" && !modelNameRe.MatchString(m) {
		errs = append(errs, "claude.deep_review_model must be a model name or alias (letters, digits, . _ : [ ] -)")
	}
	if _, ok := parseSeverity(c.Review.BlockMinSeverity); !ok {
		errs = append(errs, "review.block_min_severity must be critical, major, minor or nit")
	}
	errs = append(errs, c.Jira.validate()...)
	errs = append(errs, c.Review.Context.validate()...)
	errs = append(errs, c.Review.validatePathInstructions()...)
	errs = append(errs, c.Review.Lint.validate()...)
	errs = append(errs, c.Review.Todo.validate()...)
	errs = append(errs, c.Review.Skills.validate()...)
	errs = append(errs, c.Review.TBD.validate()...)
	errs = append(errs, c.Review.Coverage.validate()...)
	errs = append(errs, c.Review.CheckoutCache.validate()...)
	errs = append(errs, c.Review.Migrations.validate()...)
	errs = append(errs, c.Review.Sysmap.validate()...)
	if len(c.Review.Sysmap.Externals) > MaxSysmapExternals {
		errs = append(errs, fmt.Sprintf("review.sysmap.externals: at most %d entries", MaxSysmapExternals))
	}
	for i, e := range c.Review.Sysmap.Externals {
		errs = append(errs, e.validate(i, NormalizeGroups(c.GitLab.Groups))...)
	}
	errs = append(errs, c.Review.MCP.validate(c.Listen)...)
	errs = append(errs, c.Review.Problems.validate()...)
	errs = append(errs, c.Review.Reactions.validate()...)
	errs = append(errs, c.Review.Labels.validate(c.Review.BigMR.OverrideLabel)...)
	errs = append(errs, c.Review.Verify.validate()...)
	errs = append(errs, c.Review.validateBigMR()...)
	errs = append(errs, c.Review.CI.validate()...)
	errs = append(errs, c.Review.Routing.validate()...)
	errs = append(errs, c.Tracing.validate(c.Sandbox.Mode)...)
	errs = append(errs, c.Queue.validate()...)
	errs = append(errs, c.Usage.validate()...)
	if len(errs) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(errs, "; "))
	}
	return nil
}

// modelNameRe: a Claude model alias or id ("opus", "claude-opus-4-1[1m]");
// it goes into the --agents JSON and must not start an option.
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]*$`)

// ValidateSyncHooks checks only what a one-off hook sync needs.
func (c *Service) ValidateSyncHooks() error {
	var errs []string
	if c.GitLab.URL == "" {
		errs = append(errs, "gitlab.url (or GITLAB_URL) is required")
	}
	if c.GitLab.ServiceToken == "" {
		errs = append(errs, "GITLAB_SERVICE_TOKEN is required (a separate Maintainer/Owner token)")
	}
	if c.GitLab.WebhookSecret == "" {
		errs = append(errs, "GITLAB_WEBHOOK_SECRET is required (it is set on every hook)")
	}
	switch {
	case c.GitLab.WebhookURL == "":
		errs = append(errs, "gitlab.webhook_url is required")
	case !validHTTPURL(c.GitLab.WebhookURL):
		errs = append(errs, "gitlab.webhook_url must be an http(s) URL")
	}
	if len(c.GitLab.Groups) == 0 {
		errs = append(errs, "gitlab.groups must have at least one group")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(errs, "; "))
	}
	return nil
}
