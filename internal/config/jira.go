package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Jira configures the Jira Data Center / Server integration (REST API v2,
// Personal Access Token) and the MR title check. The title check works
// without Jira (URL empty).
type Jira struct {
	// URL of Jira, e.g. https://jira.example.com. Empty disables Jira; the
	// title check still works.
	URL string `yaml:"url"`
	// Token is a Personal Access Token (env JIRA_TOKEN preferred).
	Token string `yaml:"token"`
	// CAFile adds a private CA for an internal Jira.
	CAFile string `yaml:"ca_file"`
	// ProjectKeys limits which issue key prefixes are recognized, e.g.
	// ["PROJ"]. Empty accepts any prefix.
	ProjectKeys []string `yaml:"project_keys"`
	// EpicLinkField is the custom field holding the Epic Link.
	EpicLinkField string `yaml:"epic_link_field"`
	// CommentsLimit is how many of the latest issue comments go to Claude.
	CommentsLimit int `yaml:"comments_limit"`
	// TitleCheck enables the MR title hint in the summary.
	TitleCheck bool `yaml:"title_check"`
	// RequireKeyInTitle makes a missing issue key in the title a hint.
	RequireKeyInTitle bool `yaml:"require_key_in_title"`
	// TitleTypes are the allowed Conventional Commits types.
	TitleTypes []string `yaml:"title_types"`
	// TitlePrompt is the title format the model follows when it suggests
	// a title ({types}, {language} replaced; title.go). Empty = the
	// built-in DefaultTitlePrompt.
	TitlePrompt string `yaml:"title_prompt"`
	// TitleLanguage is the language of suggested titles (default "en",
	// whatever review.default_language is).
	TitleLanguage string `yaml:"title_language"`
}

// DefaultTitleTypes are the Conventional Commits types accepted by default.
var DefaultTitleTypes = []string{"feat", "fix", "refactor", "perf", "chore", "docs", "test", "build", "ci", "style", "revert"}

// Enabled reports whether Jira access is configured.
func (j Jira) Enabled() bool { return j.URL != "" }

func defaultJira() Jira {
	return Jira{
		EpicLinkField:     "customfield_10008",
		CommentsLimit:     10,
		TitleCheck:        true,
		RequireKeyInTitle: true,
		TitleTypes:        append([]string(nil), DefaultTitleTypes...),
		TitleLanguage:     DefaultTitleLanguage,
	}
}

var projectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)

// applyJiraEnv applies env overrides and normalizes the section.
func applyJiraEnv(j *Jira) {
	if v := os.Getenv("JIRA_URL"); v != "" {
		j.URL = v
	}
	if v := os.Getenv("JIRA_TOKEN"); v != "" {
		j.Token = v
	}
	j.normalize()
}

func (j *Jira) normalize() {
	j.URL = strings.TrimRight(strings.TrimSpace(j.URL), "/")
	j.Token = strings.TrimSpace(j.Token)
	j.EpicLinkField = strings.TrimSpace(j.EpicLinkField)
	keys := j.ProjectKeys[:0:0]
	for _, k := range j.ProjectKeys {
		if k = strings.ToUpper(strings.TrimSpace(k)); k != "" {
			keys = append(keys, k)
		}
	}
	j.ProjectKeys = keys
	types := j.TitleTypes[:0:0]
	for _, t := range j.TitleTypes {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			types = append(types, t)
		}
	}
	if len(types) == 0 {
		types = append(types, DefaultTitleTypes...)
	}
	j.TitleTypes = types
}

// validate returns problems with the section.
func (j *Jira) validate() []string {
	var errs []string
	if j.URL != "" {
		u, err := url.Parse(j.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, "jira.url must be http(s)://host")
		}
		if j.Token == "" {
			errs = append(errs, "jira.token (or JIRA_TOKEN) is required when jira.url is set")
		}
	}
	for _, k := range j.ProjectKeys {
		if !projectKeyRe.MatchString(k) {
			errs = append(errs, fmt.Sprintf("jira.project_keys: invalid key %q", k))
		}
	}
	if j.CommentsLimit < 0 {
		errs = append(errs, "jira.comments_limit must be >= 0")
	}
	errs = append(errs, validateTitle("jira", j.TitlePrompt, j.TitleLanguage)...)
	return errs
}

// normalizeProjectKeys upper-cases, trims and dedupes keys and rejects
// malformed ones.
func normalizeProjectKeys(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, k := range in {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k == "" || seen[k] {
			continue
		}
		if !projectKeyRe.MatchString(k) {
			return nil, fmt.Errorf("invalid key %q", k)
		}
		seen[k] = true
		out = append(out, k)
	}
	return out, nil
}
