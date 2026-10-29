package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
)

// LoadService reads path (may be empty for pure defaults + env), applies env
// overrides for secrets and validates everything `serve` needs.
func LoadService(path string) (*Service, error) {
	cfg, err := readService(path)
	if err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

// LoadSyncHooks loads the config for `reviewbot sync-hooks`. It only needs
// gitlab.url, the service token, the webhook secret (set on every hook),
// gitlab.webhook_url and gitlab.groups: the bot token (GITLAB_TOKEN) and the
// rest of the config (sandbox, Jira, review) are not validated.
func LoadSyncHooks(path string) (*Service, error) {
	cfg, err := readService(path)
	if err != nil {
		return nil, err
	}
	return cfg, cfg.ValidateSyncHooks()
}

// LoadUnvalidated reads the config like LoadService (defaults, env
// overrides) but does not validate it: `reviewbot doctor`, `init` and the
// /setup page report the problems as checks instead of failing on them.
func LoadUnvalidated(path string) (*Service, error) { return readService(path) }

// readService reads the file and applies defaults and env overrides,
// without validation.
func readService(path string) (*Service, error) {
	cfg := Defaults()
	if path != "" {
		b, err := parseServiceFile(path, &cfg)
		if err != nil {
			return nil, err
		}
		cfg.Review.Lint.removed = removedKeys(b, removedServiceLintKeys, "review", "lint") // lint.go
	}
	if v := os.Getenv("GITLAB_URL"); v != "" {
		cfg.GitLab.URL = v
	}
	if v := os.Getenv("GITLAB_TOKEN"); v != "" {
		cfg.GitLab.Token = v
	}
	if v := os.Getenv("GITLAB_WEBHOOK_SECRET"); v != "" {
		cfg.GitLab.WebhookSecret = v
	}
	if v := os.Getenv("GITLAB_HOOKS_TOKEN"); v != "" {
		cfg.GitLab.ServiceToken = v
	}
	if v := os.Getenv("GITLAB_SERVICE_TOKEN"); v != "" {
		cfg.GitLab.ServiceToken = v
	}
	cfg.GitLab.URL = strings.TrimRight(cfg.GitLab.URL, "/")
	cfg.GitLab.Groups = NormalizeGroups(cfg.GitLab.Groups)
	cfg.GitLab.Exclude = normalizeGlobs(cfg.GitLab.Exclude)
	cfg.Review.Context.Exclude = normalizeGlobs(cfg.Review.Context.Exclude)
	cfg.GitLab.WebhookURL = strings.TrimSpace(cfg.GitLab.WebhookURL)
	cfg.GitLab.Events = strings.ToLower(strings.TrimSpace(cfg.GitLab.Events))
	if cfg.GitLab.Events == "" {
		cfg.GitLab.Events = EventsWebhook
	}
	applyJiraEnv(&cfg.Jira)
	applyUsageEnv(&cfg.Usage)
	applyMetricsEnv(&cfg.Metrics)
	applyTracingEnv(&cfg.Tracing)
	return &cfg, nil
}

// parseServiceFile decodes the config file at path over cfg and refuses
// removed keys. It returns the file content.
func parseServiceFile(path string, cfg *Service) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := decodeOver(b, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := checkRemovedKeys(b, removedServiceKeys); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return b, nil
}

// decodeOver decodes the YAML document b over cfg, which holds the
// defaults. A document without content (empty, only comments, a bare "---"
// or null) keeps them: go-yaml would reset cfg to the zero value (limits
// off, hook_ssl_verify false, no sandbox mode).
func decodeOver(b []byte, cfg *Service) error {
	var doc any
	if yaml.Unmarshal(b, &doc) == nil && doc == nil {
		return nil
	}
	return yaml.Unmarshal(b, cfg)
}

// LoadSandbox reads only the sandbox section (defaults applied), without
// secret validation. Used by the egress subcommand.
func LoadSandbox(path string) (*Sandbox, error) {
	cfg := Defaults()
	if _, err := parseServiceFile(path, &cfg); err != nil {
		return nil, err
	}
	// tracing.claude_cli with an https collector: the sandbox reaches it
	// through the egress proxy (only then; see tracing.go).
	applyTracingEnv(&cfg.Tracing)
	if h := cfg.Tracing.CLIEgressHost(); h != "" && !containsFold(cfg.Sandbox.EgressAllow, h) {
		cfg.Sandbox.EgressAllow = append(cfg.Sandbox.EgressAllow, h)
	}
	return &cfg.Sandbox, nil
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), s) {
			return true
		}
	}
	return false
}
