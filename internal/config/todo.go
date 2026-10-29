package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// ReviewTodo is review.todo: the built-in "todo" check. TODO/FIXME/XXX/HACK
// comments added by a merge request must reference a task (a Jira key, a
// GitLab issue or a link to one); otherwise the bot opens a lint thread
// asking to link one. Deterministic, runs in the bot (no sandbox, no
// Claude); see internal/lint/todo.go.
type ReviewTodo struct {
	// Enabled turns the check on for the whole service (default true).
	Enabled bool `yaml:"enabled"`
	// Keywords are the task markers (uppercase, whole word; default TODO,
	// FIXME, XXX, HACK).
	Keywords []string `yaml:"keywords"`
	// RequireTask: a marker without a task reference is a finding (default
	// true). false leaves only verify_keys.
	RequireTask bool `yaml:"require_task"`
	// Severity of "TODO без задачи" (default minor).
	Severity string `yaml:"severity"`
	// Blocking: results follow review.block_min_severity like custom
	// linters with blocking: true (default false: never block).
	Blocking bool `yaml:"blocking"`
	// Ignore are doublestar globs of files the check skips (vendored code,
	// minified files); files with a "generated" header are always skipped.
	Ignore []string `yaml:"ignore"`
	// VerifyKeys: when Jira is configured, look the referenced keys up
	// (read-only, at most MaxTodoKeyLookups per review, cached) and report
	// keys Jira does not know.
	VerifyKeys bool `yaml:"verify_keys"`
}

// Todo check limits and defaults.
const (
	// MaxTodoKeyLookups caps Jira lookups of verify_keys per review.
	MaxTodoKeyLookups = 10
	maxTodoKeywords   = 20
	maxTodoIgnore     = 100
)

// DefaultTodoKeywords are the default task markers.
var DefaultTodoKeywords = []string{"TODO", "FIXME", "XXX", "HACK"}

// DefaultTodoIgnore are the default globs the todo check skips.
var DefaultTodoIgnore = []string{"vendor/**", "node_modules/**", "**/*.min.js", "**/*.min.css"}

var todoKeywordRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,19}$`)

func defaultReviewTodo() ReviewTodo {
	return ReviewTodo{
		Enabled:     true,
		Keywords:    append([]string{}, DefaultTodoKeywords...),
		RequireTask: true,
		Severity:    "minor",
		Ignore:      append([]string{}, DefaultTodoIgnore...),
	}
}

func (c ReviewTodo) validate() []string {
	var errs []string
	if err := checkTodoKeywords(c.Keywords); err != "" {
		errs = append(errs, "review.todo.keywords: "+err)
	}
	if c.Enabled && len(c.Keywords) == 0 {
		errs = append(errs, "review.todo.keywords must not be empty (use review.todo.enabled: false to turn the check off)")
	}
	if _, ok := parseSeverity(c.Severity); !ok {
		errs = append(errs, "review.todo.severity must be critical, major, minor or nit")
	}
	if _, err := normalizeTodoIgnore(c.Ignore); err != nil {
		errs = append(errs, "review.todo.ignore: "+err.Error())
	}
	return errs
}

func checkTodoKeywords(kws []string) string {
	if len(kws) > maxTodoKeywords {
		return fmt.Sprintf("at most %d keywords", maxTodoKeywords)
	}
	for _, k := range kws {
		if !todoKeywordRe.MatchString(strings.TrimSpace(k)) {
			return fmt.Sprintf("%q must be 2-20 uppercase letters, digits or _ starting with a letter (markers are matched case-sensitively)", k)
		}
	}
	return ""
}

func normalizeTodoIgnore(in []string) ([]string, error) {
	if len(in) > maxTodoIgnore {
		return nil, fmt.Errorf("at most %d globs", maxTodoIgnore)
	}
	var out []string
	for _, g := range in {
		g = strings.TrimPrefix(strings.TrimSpace(g), "./")
		if g == "" {
			continue
		}
		if strings.HasPrefix(g, "/") || !doublestar.ValidatePattern(g) {
			return nil, fmt.Errorf("invalid glob %q", g)
		}
		out = append(out, g)
	}
	return out, nil
}

// RepoTodo is the todo section of .reviewbot.yaml.
//
// A repository may turn the check off, replace the keywords, change the
// severity and add ignore globs. Blocking can only be made stricter:
// blocking: true turns it on for the repository, blocking: false does not
// turn off a blocking check of the service. require_task and verify_keys
// are service-only.
type RepoTodo struct {
	// Enabled=false turns the check off for the repository.
	Enabled *bool `yaml:"enabled"`
	// Keywords replace review.todo.keywords.
	Keywords []string `yaml:"keywords"`
	// Severity replaces review.todo.severity.
	Severity string `yaml:"severity"`
	// Blocking=true makes the results blocking (cannot relax the service).
	Blocking *bool `yaml:"blocking"`
	// Ignore globs are added to review.todo.ignore.
	Ignore []string `yaml:"ignore"`
}

func (c *RepoTodo) normalize() error {
	if c == nil {
		return nil
	}
	var kws []string
	for _, k := range c.Keywords {
		if k = strings.TrimSpace(k); k != "" {
			kws = append(kws, k)
		}
	}
	if err := checkTodoKeywords(kws); err != "" {
		return fmt.Errorf("todo.keywords: %s", err)
	}
	c.Keywords = kws
	c.Severity = strings.ToLower(strings.TrimSpace(c.Severity))
	if c.Severity != "" {
		if _, ok := parseSeverity(c.Severity); !ok {
			return fmt.Errorf("todo.severity must be critical, major, minor or nit")
		}
	}
	ig, err := normalizeTodoIgnore(c.Ignore)
	if err != nil {
		return fmt.Errorf("todo.ignore: %w", err)
	}
	c.Ignore = ig
	return nil
}

// Todo is the effective todo check configuration of one repository.
type Todo struct {
	// Enabled: the service and the repository have it on and there is
	// something to check (require_task or verify_keys).
	Enabled     bool
	Keywords    []string
	RequireTask bool
	Severity    core.Severity
	Blocking    bool
	Ignore      []string
	VerifyKeys  bool
}

// mergeTodo builds the effective todo config. Safe on a nil *Repo.
func mergeTodo(c ReviewTodo, repo *Repo) Todo {
	t := Todo{RequireTask: c.RequireTask, Blocking: c.Blocking, VerifyKeys: c.VerifyKeys}
	t.Severity, _ = parseSeverity(c.Severity)
	if t.Severity == "" {
		t.Severity = core.SeverityMinor
	}
	for _, k := range c.Keywords {
		if k = strings.TrimSpace(k); k != "" {
			t.Keywords = append(t.Keywords, k)
		}
	}
	t.Ignore, _ = normalizeTodoIgnore(c.Ignore)
	on := c.Enabled
	if repo != nil && repo.Todo != nil {
		rt := repo.Todo
		on = on && defaultTrue(rt.Enabled)
		if len(rt.Keywords) > 0 {
			t.Keywords = append([]string{}, rt.Keywords...)
		}
		if s, ok := parseSeverity(rt.Severity); ok {
			t.Severity = s
		}
		t.Blocking = t.Blocking || setTrue(rt.Blocking)
		t.Ignore = append(t.Ignore, rt.Ignore...)
	}
	t.Enabled = on && len(t.Keywords) > 0 && (t.RequireTask || t.VerifyKeys)
	return t
}
