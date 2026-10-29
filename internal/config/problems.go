package config

import (
	"fmt"
	"strings"
	"time"
)

// Problems configures the verification problems (review.problems, see
// internal/problems): what the bot could not check during a review
// (a linter that did not run, a missing coverage report, a related
// repository without access, a cut diff, an answer that could not be
// parsed, and what the review model reports it could not verify). They are
// listed on /problems and in the MR summary of the run that hit them.
type Problems struct {
	// Enabled records problems, shows them in summaries and on /problems,
	// and lets the model report them (default true).
	Enabled bool `yaml:"enabled"`
	// Notify are GitLab usernames (without "@") mentioned when a review hits
	// a new problem (or one that re-appeared after an acknowledgement, or
	// was last mentioned longer ago than RenotifyAfter). Empty = nobody.
	Notify []string `yaml:"notify"`
	// NotifyMode: "summary" (default) adds the mention to the problems block
	// of the summary note; "note" posts a separate short note in the MR
	// (use it if GitLab does not notify on mentions added by editing a note).
	NotifyMode string `yaml:"notify_mode"`
	// RenotifyAfter: a problem seen again is mentioned again once this long
	// has passed since the last mention (default 168h).
	RenotifyAfter Duration `yaml:"renotify_after"`
	// PageURL is the bot's external base URL (e.g. https://reviewbot.example.com);
	// the summary links to <page_url>/problems. Empty = no link.
	PageURL string `yaml:"page_url"`
	// Retention: problems not seen for this long are deleted (default 2160h
	// = 90 days).
	Retention Duration `yaml:"retention"`
}

// Problems defaults.
const (
	DefaultProblemsRenotify  = 168 * time.Hour
	DefaultProblemsRetention = 90 * 24 * time.Hour
	MaxProblemsNotify        = 20
)

func defaultProblems() Problems {
	return Problems{Enabled: true, NotifyMode: "summary",
		RenotifyAfter: Duration(DefaultProblemsRenotify), Retention: Duration(DefaultProblemsRetention)}
}

// NotifyUsers returns the usernames without "@" and blanks.
func (p Problems) NotifyUsers() []string { return trimUsernames(p.Notify) }

// Mode returns the notify mode ("summary" when unset).
func (p Problems) Mode() string {
	if m := strings.ToLower(strings.TrimSpace(p.NotifyMode)); m != "" {
		return m
	}
	return "summary"
}

// RenotifyD returns renotify_after (default 168h).
func (p Problems) RenotifyD() time.Duration {
	return positiveOr(p.RenotifyAfter.D(), DefaultProblemsRenotify)
}

// RetentionD returns the retention (default 90 days).
func (p Problems) RetentionD() time.Duration {
	return positiveOr(p.Retention.D(), DefaultProblemsRetention)
}

func (p Problems) validate() []string {
	var errs []string
	users := p.NotifyUsers()
	if len(users) > MaxProblemsNotify {
		errs = append(errs, fmt.Sprintf("review.problems.notify: at most %d usernames", MaxProblemsNotify))
	}
	for _, u := range users {
		if !validGitLabUsername(u) {
			errs = append(errs, fmt.Sprintf("review.problems.notify: %q is not a GitLab username (letters, digits, _, - and .)", u))
		}
	}
	switch p.Mode() {
	case "summary", "note":
	default:
		errs = append(errs, `review.problems.notify_mode must be "summary" or "note"`)
	}
	if p.RenotifyAfter.D() < 0 || (p.RenotifyAfter.D() > 0 && p.RenotifyAfter.D() < time.Hour) {
		errs = append(errs, "review.problems.renotify_after must be >= 1h")
	}
	if p.Retention.D() < 0 || (p.Retention.D() > 0 && p.Retention.D() < 24*time.Hour) {
		errs = append(errs, "review.problems.retention must be >= 24h")
	}
	if u := strings.TrimSpace(p.PageURL); u != "" && !validBaseURL(u) {
		errs = append(errs, "review.problems.page_url must be an http(s) URL without credentials, query or fragment, e.g. https://reviewbot.example.com")
	}
	return errs
}

// RepoProblems is the problems section of .reviewbot.yaml: a repository may
// only hide the problems block (and its mentions) in its MR summaries;
// problems are still recorded and listed on /problems.
type RepoProblems struct {
	Summary *bool `yaml:"summary"`
}

func (r *Repo) problemsSummary() bool {
	return r == nil || r.Problems == nil || defaultTrue(r.Problems.Summary)
}
