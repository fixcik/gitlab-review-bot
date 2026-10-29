package config

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Lint tool names the bot runs itself (see internal/lint). Linters are not
// among them: their results come from the repository's CI (lint.ci).
const (
	LintGitleaks = "gitleaks"
)

// LintTools is every tool the bot runs itself.
var LintTools = []string{LintGitleaks}

func knownLintTool(t string) bool { return slices.Contains(LintTools, t) }

// removedLintTools are the linters the bot used to run itself. Their names
// in review.lint.tools, review.lint.severity_overrides or lint.disable are a
// configuration error pointing at lint.ci.
var removedLintTools = []string{"semgrep", "golangci-lint", "ruff", "eslint", "phpstan", "phpcs"}

// LintRemovedHint ends the error about a removed lint key or tool.
const LintRemovedHint = "removed: linters now come from CI reports — configure `lint.ci`, see docs/features/lint.md"

func removedLintTool(t string) bool { return t == "custom" || slices.Contains(removedLintTools, t) }

// Removed keys: service review.lint.* and .reviewbot.yaml lint.*.
var (
	removedServiceLintKeys = []string{"semgrep_rules", "allow_custom"}
	removedRepoLintKeys    = []string{"semgrep_configs", "custom"}
)

// removedKeys returns the keys of the map at path (e.g. "review", "lint")
// that are in removed, sorted. Unparsable input yields nothing (the real
// decoder reports it).
func removedKeys(b []byte, removed []string, path ...string) []string {
	var doc map[string]any
	if yaml.Unmarshal(b, &doc) != nil {
		return nil
	}
	cur := doc
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	var out []string
	for _, k := range removed {
		if _, ok := cur[k]; ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ReviewLint is review.lint: the secret scanner the bot runs itself
// (gitleaks) and the linter reports of the repositories' CI (lint.ci),
// whose results the bot publishes itself (only on lines the MR changes).
type ReviewLint struct {
	// Enabled turns the feature on for the whole service.
	Enabled bool `yaml:"enabled"`
	// Timeout bounds each tool run.
	Timeout Duration `yaml:"timeout"`
	// Tools the bot may run itself (gitleaks).
	Tools []string `yaml:"tools"`
	// SeverityOverrides: "tool" or "tool:rule" -> critical|major|minor|nit;
	// CI linters as "ci:<tool>" or "ci:<tool>:<rule>". gitleaks results are
	// always critical.
	SeverityOverrides map[string]string `yaml:"severity_overrides"`
	// AllowDisableSecrets lets a repository turn gitleaks off (default
	// false: secret scanning cannot be disabled from .reviewbot.yaml).
	AllowDisableSecrets bool `yaml:"allow_disable_secrets"`

	// removed are review.lint keys of the bot's former linters found in
	// the file (set by readService, reported by validate).
	removed []string
}

func defaultReviewLint() ReviewLint {
	return ReviewLint{
		Enabled: true,
		Timeout: Duration(3 * time.Minute),
		Tools:   append([]string{}, LintTools...),
	}
}

func (c ReviewLint) validate() []string {
	var errs []string
	for _, k := range c.removed {
		errs = append(errs, fmt.Sprintf("review.lint.%s: %s", k, LintRemovedHint))
	}
	for _, t := range c.Tools {
		switch n := strings.ToLower(strings.TrimSpace(t)); {
		case removedLintTool(n):
			errs = append(errs, fmt.Sprintf("review.lint.tools: %q %s", t, LintRemovedHint))
		case !knownLintTool(n):
			errs = append(errs, fmt.Sprintf("review.lint.tools: unknown tool %q", t))
		}
	}
	keys := make([]string, 0, len(c.SeverityOverrides))
	for k := range c.SeverityOverrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		tool, rest, _ := strings.Cut(strings.TrimSpace(k), ":")
		switch t := strings.ToLower(tool); {
		case t == "ci":
			name, _, _ := strings.Cut(rest, ":")
			if !ciToolRe.MatchString(strings.ToLower(name)) {
				errs = append(errs, fmt.Sprintf("review.lint.severity_overrides: %q must be \"ci:<tool>\" or \"ci:<tool>:<rule>\"", k))
			}
		case removedLintTool(t):
			errs = append(errs, fmt.Sprintf("review.lint.severity_overrides: %q %s (key \"ci:%s\")", k, LintRemovedHint, t))
		case !knownLintTool(t):
			errs = append(errs, fmt.Sprintf("review.lint.severity_overrides: unknown tool in %q", k))
		}
		if _, ok := parseSeverity(c.SeverityOverrides[k]); !ok {
			errs = append(errs, fmt.Sprintf("review.lint.severity_overrides[%q] must be critical, major, minor or nit", k))
		}
	}
	if c.Enabled && c.Timeout.D() < 10*time.Second {
		errs = append(errs, "review.lint.timeout must be >= 10s")
	}
	return errs
}

func parseSeverity(s string) (core.Severity, bool) {
	switch v := core.Severity(strings.ToLower(strings.TrimSpace(s))); v {
	case core.SeverityCritical, core.SeverityMajor, core.SeverityMinor, core.SeverityNit:
		return v, true
	}
	return "", false
}

// RepoLint is the lint section of .reviewbot.yaml.
type RepoLint struct {
	// Enabled=false turns off everything except gitleaks (which stays on
	// unless review.lint.allow_disable_secrets), CI linters included.
	Enabled *bool `yaml:"enabled"`
	// Disable lists tools not to run for this repository (gitleaks).
	Disable []string `yaml:"disable"`
	// CI are the linter reports of the repository's own CI jobs, read from
	// the MR head pipeline.
	CI []CILint `yaml:"ci"`
}

// CILint is one entry of lint.ci: a CI job of the MR head pipeline and
// where its linter report is.
type CILint struct {
	// Job is the job name; a glob (path.Match: *, ?, [..]) is allowed.
	// Parallel instances ("phpstan 1/2") match the plain name.
	Job string `yaml:"job"`
	// Artifact is the report's path inside the job artifacts archive.
	Artifact string `yaml:"artifact"`
	// Report: "codequality" reads the job's GitLab Code Quality report
	// (artifacts:reports:codequality) instead of an artifact path.
	Report string `yaml:"report"`
	// Format: auto (default) | codeclimate | sarif | checkstyle.
	Format string `yaml:"format"`
	// Tool is the shown name (ci:<tool>); default: the job name.
	Tool string `yaml:"tool"`
	// Severity for every result; empty = from the report.
	Severity string `yaml:"severity"`
	// Blocking: results follow review.block_min_severity instead of never
	// blocking the merge.
	Blocking bool `yaml:"blocking"`
}

// CI report kinds and formats (lint.ci[].report, lint.ci[].format).
const (
	CIReportCodeQuality = "codequality"
	CIFormatAuto        = "auto"
)

var ciFormats = []string{CIFormatAuto, "codeclimate", "sarif", "checkstyle"}

// MaxCILints caps lint.ci.
const MaxCILints = 10

var (
	ciToolRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	ciNameBad = regexp.MustCompile(`[^a-z0-9._-]+`)
)

// ciToolName derives the tool name from a job name: lowercase, glob and
// other characters dropped or replaced by "-".
func ciToolName(job string) string {
	s := strings.ToLower(job)
	s = strings.NewReplacer("*", "", "?", "", "[", "", "]", "").Replace(s)
	s = strings.Trim(ciNameBad.ReplaceAllString(s, "-"), "-._")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-._")
	}
	return s
}

func (c *RepoLint) normalize() error {
	if c == nil {
		return nil
	}
	var dis []string
	for _, t := range c.Disable {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if removedLintTool(t) {
			return fmt.Errorf("lint.disable: %q %s", t, LintRemovedHint)
		}
		if !knownLintTool(t) {
			return fmt.Errorf("lint.disable: unknown tool %q (known: %s)", t, strings.Join(LintTools, ", "))
		}
		dis = append(dis, t)
	}
	c.Disable = dis
	if len(c.CI) > MaxCILints {
		return fmt.Errorf("lint.ci: at most %d entries", MaxCILints)
	}
	seen := map[string]bool{}
	for i := range c.CI {
		e := &c.CI[i]
		e.Job = strings.TrimSpace(e.Job)
		e.Artifact = strings.TrimPrefix(strings.TrimSpace(e.Artifact), "./")
		e.Report = strings.ToLower(strings.TrimSpace(e.Report))
		e.Format = strings.ToLower(strings.TrimSpace(e.Format))
		e.Tool = strings.ToLower(strings.TrimSpace(e.Tool))
		e.Severity = strings.ToLower(strings.TrimSpace(e.Severity))
		if e.Format == "" {
			e.Format = CIFormatAuto
		}
		if e.Tool == "" {
			e.Tool = ciToolName(e.Job)
		}
		where := fmt.Sprintf("lint.ci[%d]", i)
		if _, err := path.Match(e.Job, ""); e.Job == "" || err != nil || len(e.Job) > 255 {
			return fmt.Errorf("%s: job must be a job name or glob", where)
		}
		switch {
		case (e.Artifact == "") == (e.Report == ""):
			return fmt.Errorf("%s (%s): set exactly one of artifact (a path in the job artifacts) and report: codequality", where, e.Job)
		case e.Report != "" && e.Report != CIReportCodeQuality:
			return fmt.Errorf("%s (%s): report must be %q", where, e.Job, CIReportCodeQuality)
		case e.Report != "" && e.Format != CIFormatAuto && e.Format != "codeclimate":
			return fmt.Errorf("%s (%s): the codequality report is CodeClimate JSON (format auto or codeclimate)", where, e.Job)
		case e.Artifact != "" && (strings.HasPrefix(e.Artifact, "/") || strings.Contains(e.Artifact, "..") || path.Clean(e.Artifact) != e.Artifact):
			return fmt.Errorf("%s (%s): artifact must be a relative path inside the job artifacts", where, e.Job)
		case !slices.Contains(ciFormats, e.Format):
			return fmt.Errorf("%s (%s): format must be one of %s", where, e.Job, strings.Join(ciFormats, ", "))
		case !ciToolRe.MatchString(e.Tool):
			return fmt.Errorf("%s (%s): tool %q must be lowercase letters, digits, '.', '_' or '-' (up to 32)", where, e.Job, e.Tool)
		case seen[e.Tool]:
			return fmt.Errorf("%s (%s): duplicate tool %q (set tool: to tell the entries apart)", where, e.Job, e.Tool)
		}
		if e.Severity != "" {
			if _, ok := parseSeverity(e.Severity); !ok {
				return fmt.Errorf("%s (%s): severity must be critical, major, minor or nit", where, e.Job)
			}
		}
		seen[e.Tool] = true
	}
	return nil
}

// Lint is the effective lint configuration of one repository.
type Lint struct {
	// Enabled: gitleaks runs or at least one CI linter is configured.
	Enabled bool
	// Tools the bot runs itself, in no particular order.
	Tools   []string
	Timeout time.Duration
	// SeverityOverrides with validated severities ("ci:<tool>[:rule]" for
	// CI linters).
	SeverityOverrides map[string]core.Severity
	// SecretsKept: the repository asked to turn gitleaks off but the
	// service does not allow it.
	SecretsKept bool
	// CI are the repository's CI linter reports (validated); empty when
	// lint is off for the repository.
	CI []CILint
}

// mergeLint builds the effective lint config. Safe on a nil *Repo.
func mergeLint(svc *Service, repo *Repo) Lint {
	c := svc.Review.Lint
	l := Lint{Timeout: c.Timeout.D(), SeverityOverrides: map[string]core.Severity{}}
	if !c.Enabled {
		return l
	}
	for k, v := range c.SeverityOverrides {
		if s, ok := parseSeverity(v); ok {
			tool, rule, hasRule := strings.Cut(strings.TrimSpace(k), ":")
			key := strings.ToLower(tool)
			if key == "ci" && hasRule {
				name, r, more := strings.Cut(rule, ":")
				key += ":" + strings.ToLower(name)
				rule, hasRule = r, more
			}
			if hasRule {
				key += ":" + rule
			}
			l.SeverityOverrides[key] = s
		}
	}
	off := map[string]bool{}
	var rl *RepoLint
	if repo != nil {
		rl = repo.Lint
	}
	if rl != nil {
		if defaultTrue(rl.Enabled) {
			l.CI = append([]CILint{}, rl.CI...)
		} else {
			for _, t := range LintTools {
				off[t] = true
			}
		}
		for _, t := range rl.Disable {
			off[t] = true
		}
	}
	if off[LintGitleaks] && !c.AllowDisableSecrets {
		off[LintGitleaks] = false
		l.SecretsKept = true
	}
	seen := map[string]bool{}
	for _, t := range c.Tools {
		t = strings.ToLower(strings.TrimSpace(t))
		if !knownLintTool(t) || off[t] || seen[t] {
			continue
		}
		seen[t] = true
		l.Tools = append(l.Tools, t)
	}
	if !seen[LintGitleaks] {
		l.SecretsKept = false // the service does not run gitleaks at all
	}
	l.Enabled = len(l.Tools) > 0 || len(l.CI) > 0
	return l
}
