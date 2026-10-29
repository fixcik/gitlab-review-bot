package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Small helpers shared by the config sections.

// defaultTrue reads an optional switch that is on when not set (a repo
// section's enabled: only an explicit false turns the feature off).
func defaultTrue(p *bool) bool { return p == nil || *p }

// setTrue reports whether an optional switch is explicitly true (a repo
// setting that can only make the service stricter, e.g. blocking: true).
func setTrue(p *bool) bool { return p != nil && *p }

// positiveOr returns v, or def when v is not positive (unset).
func positiveOr[T ~int | ~int64](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

// gitlabUsernameRe: GitLab usernames are letters, digits, "_", "-" and ".".
var gitlabUsernameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,254}$`)

// validGitLabUsername reports whether u (without "@") can be a GitLab
// username.
func validGitLabUsername(u string) bool {
	return gitlabUsernameRe.MatchString(u) && !strings.HasSuffix(u, ".") && !strings.Contains(u, "..")
}

// trimUsernames returns the usernames without "@" and blanks.
func trimUsernames(list []string) []string {
	var out []string
	for _, u := range list {
		if u = strings.TrimPrefix(strings.TrimSpace(u), "@"); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// validBaseURL reports whether s is an http(s) URL with a host and without
// credentials, query or fragment (a base URL the bot builds links on).
func validBaseURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// pageAccessToken is the token protecting a page of the bot: the env
// variable, else the section's own token, else the usage page token.
// Empty = open.
func pageAccessToken(env, own, usageToken string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	if v := strings.TrimSpace(own); v != "" {
		return v
	}
	return strings.TrimSpace(usageToken)
}

// cleanGlobList trims spaces and slashes, drops empty entries and rejects
// the first invalid doublestar glob.
func cleanGlobList(in []string) ([]string, error) {
	out := normalizeGlobs(in)
	for _, g := range out {
		if !doublestar.ValidatePattern(g) {
			return nil, fmt.Errorf("invalid glob %q", g)
		}
	}
	return out, nil
}
