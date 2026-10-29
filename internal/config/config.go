// Package config loads the service config (config.yaml, owned by the bot
// operator) and the per-repository .reviewbot.yaml (owned by the project,
// read from the MR target branch).
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/goccy/go-yaml"
)

// RepoConfigPath is where the per-repository config lives.
const RepoConfigPath = ".reviewbot.yaml"

// Duration is a time.Duration that unmarshals from "10m", "90s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

func validHTTPURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// matchGlobs matches a project path against doublestar globs, ignoring case.
func matchGlobs(globs []string, projectPath string) bool {
	p := strings.ToLower(strings.Trim(strings.TrimSpace(projectPath), "/"))
	if p == "" {
		return false
	}
	for _, g := range globs {
		if ok, err := doublestar.Match(strings.ToLower(g), p); err == nil && ok {
			return true
		}
	}
	return false
}

// normalizeGlobs trims spaces and slashes and drops empty entries.
func normalizeGlobs(in []string) []string {
	var out []string
	for _, g := range in {
		if g = strings.Trim(strings.TrimSpace(g), "/"); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func validateGlobs(key string, globs []string) []string {
	var errs []string
	for _, g := range globs {
		if !doublestar.ValidatePattern(g) {
			errs = append(errs, fmt.Sprintf("%s: invalid glob %q", key, g))
		}
	}
	return errs
}
