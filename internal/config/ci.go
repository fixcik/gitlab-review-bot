package config

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// CI configures the CI failure digest (review.ci, see internal/review/pipeline/ci.go
// and docs/features/ci.md): when the head pipeline of an open MR fails, the
// summary gets a digest of the failed jobs (failed tests from the JUnit
// report, the end of the job log otherwise) and, with Explain, one Claude
// run explains why it failed in a bot thread; when a pipeline of a main
// branch fails (MainBranch), the merged MR that most likely broke it gets a
// note.
type CI struct {
	// Enabled turns the feature on (default true). A repository can turn
	// it off (.reviewbot.yaml ci.enabled: false).
	Enabled bool `yaml:"enabled"`
	// Explain asks Claude why the pipeline failed (one run per failed
	// pipeline, default true). Off: only the digest.
	Explain bool `yaml:"explain"`
	// Model of the explanation run; empty = claude.light_model when set,
	// else claude.model.
	Model string `yaml:"model"`
	// MaxLogLines: lines of a failed job's log shown in the digest (default
	// 40).
	MaxLogLines int `yaml:"max_log_lines"`
	// MaxTests: failed test cases listed per pipeline (default 10).
	MaxTests int `yaml:"max_tests"`
	// MainBranch watches the pipelines of each repository's main branch
	// (gitlab.main_branch / main_branches, else the default branch): a
	// failure is reported in the merged MR that most likely broke it
	// (default true).
	MainBranch bool `yaml:"main_branch"`
	// MainSources: the pipeline sources (GitLab's pipeline "source") whose
	// pipelines of a main branch count as "main broke" / "main is green"
	// (default ["push"]: a merge or a push). Scheduled, manual (web), API,
	// trigger pipelines run other things (nightly e2e, deploys) and do not
	// blame the last merged MR unless listed; child pipelines
	// (parent_pipeline) never count. Empty = the default.
	MainSources []string `yaml:"main_sources"`
	// IgnoreJobs: job name globs (path.Match, e.g. "deploy-*") whose
	// failures are ignored. The repository's ci.ignore_jobs are added.
	IgnoreJobs []string `yaml:"ignore_jobs"`
	// NotifyMain: GitLab usernames (without "@") mentioned when a main
	// branch breaks (in the note of the culprit MR, or in the comment on the
	// commit of a direct push).
	NotifyMain []string `yaml:"notify_main"`
}

// CI defaults and bounds.
const (
	DefaultCIMaxLogLines = 40
	DefaultCIMaxTests    = 10
	MaxCIMaxLogLines     = 200
	MaxCIMaxTests        = 50
	MaxCIIgnoreJobs      = 50
	MaxCINotifyMain      = 20
)

// CIPipelineSources are GitLab's pipeline sources (Ci::Pipeline sources) that
// review.ci.main_sources accepts. parent_pipeline (a child pipeline) is not
// among them: the parent's own pipeline is what counts.
var CIPipelineSources = []string{"push", "web", "trigger", "schedule", "api", "external", "pipeline", "chat", "webide",
	"merge_request_event", "external_pull_request_event", "ondemand_dast_scan", "ondemand_dast_validation",
	"security_orchestration_policy", "container_registry_push", "duo_workflow", "pipeline_execution_policy_schedule", "unknown"}

// ChildPipelineSource is the source of a child pipeline (trigger:include).
const ChildPipelineSource = "parent_pipeline"

// DefaultCIMainSources: only pipelines of a push (a merge is a push to the
// main branch) say whether main is broken.
var DefaultCIMainSources = []string{"push"}

func defaultCI() CI {
	return CI{Enabled: true, Explain: true, MaxLogLines: DefaultCIMaxLogLines, MaxTests: DefaultCIMaxTests, MainBranch: true,
		MainSources: append([]string(nil), DefaultCIMainSources...)}
}

// MainSourceAllowed reports whether a main branch pipeline of this source
// drives the main state (review.ci.main_sources). A child pipeline never
// does; an unknown source ("": the API did not say) is allowed.
func (c CI) MainSourceAllowed(source string) bool {
	source = strings.TrimSpace(source)
	switch source {
	case ChildPipelineSource:
		return false
	case "":
		return true
	}
	list := c.MainSources
	if len(list) == 0 {
		list = DefaultCIMainSources
	}
	for _, s := range list {
		if strings.TrimSpace(s) == source {
			return true
		}
	}
	return false
}

// NotifyUsers returns notify_main without "@" and blanks.
func (c CI) NotifyUsers() []string { return trimUsernames(c.NotifyMain) }

// LogLines returns max_log_lines (default 40).
func (c CI) LogLines() int { return positiveOr(c.MaxLogLines, DefaultCIMaxLogLines) }

// Tests returns max_tests (default 10).
func (c CI) Tests() int { return positiveOr(c.MaxTests, DefaultCIMaxTests) }

// CIExplainModel is the model of the explanation run: review.ci.model, else
// claude.light_model, else claude.model.
func (s *Service) CIExplainModel() string {
	for _, m := range []string{s.Review.CI.Model, s.Claude.LightModel, s.Claude.Model} {
		if m = strings.TrimSpace(m); m != "" {
			return m
		}
	}
	return ""
}

func validJobGlobs(key string, globs []string) []string {
	var errs []string
	if len(globs) > MaxCIIgnoreJobs {
		errs = append(errs, fmt.Sprintf("%s: at most %d globs", key, MaxCIIgnoreJobs))
	}
	for _, g := range globs {
		if strings.TrimSpace(g) == "" {
			errs = append(errs, key+": empty glob")
			continue
		}
		if _, err := path.Match(g, ""); err != nil {
			errs = append(errs, fmt.Sprintf("%s: invalid glob %q", key, g))
		}
	}
	return errs
}

func (c CI) validate() []string {
	var errs []string
	if c.MaxLogLines < 5 || c.MaxLogLines > MaxCIMaxLogLines {
		errs = append(errs, fmt.Sprintf("review.ci.max_log_lines must be between 5 and %d", MaxCIMaxLogLines))
	}
	if c.MaxTests < 1 || c.MaxTests > MaxCIMaxTests {
		errs = append(errs, fmt.Sprintf("review.ci.max_tests must be between 1 and %d", MaxCIMaxTests))
	}
	errs = append(errs, validJobGlobs("review.ci.ignore_jobs", c.IgnoreJobs)...)
	for _, s := range c.MainSources {
		switch s = strings.TrimSpace(s); {
		case s == ChildPipelineSource:
			errs = append(errs, "review.ci.main_sources: parent_pipeline (child pipelines) never counts for main: the parent pipeline does")
		case !slices.Contains(CIPipelineSources, s):
			errs = append(errs, fmt.Sprintf("review.ci.main_sources: unknown pipeline source %q (one of %s)", s, strings.Join(CIPipelineSources, ", ")))
		}
	}
	users := c.NotifyUsers()
	if len(users) > MaxCINotifyMain {
		errs = append(errs, fmt.Sprintf("review.ci.notify_main: at most %d usernames", MaxCINotifyMain))
	}
	for _, u := range users {
		if !validGitLabUsername(u) {
			errs = append(errs, fmt.Sprintf("review.ci.notify_main: %q is not a GitLab username (letters, digits, _, - and .)", u))
		}
	}
	if strings.ContainsAny(c.Model, " \t\n") {
		errs = append(errs, "review.ci.model must be a model name without spaces")
	}
	return errs
}

// RepoCI is the ci section of .reviewbot.yaml: a repository may turn the
// CI failure digest off and ignore more jobs.
type RepoCI struct {
	// Enabled=false: no digest, no explanation, no main branch notes for
	// this repository (it cannot turn the feature on).
	Enabled *bool `yaml:"enabled"`
	// IgnoreJobs: job name globs (path.Match, e.g. "deploy-*") whose
	// failures are not analysed, added to review.ci.ignore_jobs.
	IgnoreJobs []string `yaml:"ignore_jobs"`
}

func (r *RepoCI) normalize() error {
	if r == nil {
		return nil
	}
	var out []string
	for _, g := range r.IgnoreJobs {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	r.IgnoreJobs = out
	if errs := validJobGlobs("ci.ignore_jobs", out); len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// EffCI is the effective CI failure digest configuration.
type EffCI struct {
	// Enabled: review.ci.enabled AND ci.enabled of the repository.
	Enabled bool
	Explain bool
	// MainBranch: watch the main branch pipelines (service only).
	MainBranch bool
	// IgnoreJobs: the service globs, then the repository's.
	IgnoreJobs  []string
	MaxLogLines int
	MaxTests    int
}

func mergeCI(svc CI, repo *Repo) EffCI {
	e := EffCI{Enabled: svc.Enabled, Explain: svc.Explain, MainBranch: svc.MainBranch,
		IgnoreJobs: append([]string(nil), svc.IgnoreJobs...), MaxLogLines: svc.LogLines(), MaxTests: svc.Tests()}
	if repo != nil && repo.CI != nil {
		e.Enabled = e.Enabled && defaultTrue(repo.CI.Enabled)
		e.IgnoreJobs = append(e.IgnoreJobs, repo.CI.IgnoreJobs...)
	}
	return e
}

// IgnoredJob reports whether a job name matches one of the ignore globs.
func (e EffCI) IgnoredJob(name string) bool {
	// Parallel jobs are named "e2e 1/4": the pattern also matches the base
	// name ("e2e"), like lint.ci job patterns.
	base := name
	if i := strings.LastIndexByte(name, ' '); i > 0 && parallelSuffix(name[i+1:]) {
		base = name[:i]
	}
	for _, g := range e.IgnoreJobs {
		m1, _ := path.Match(g, name)
		m2, _ := path.Match(g, base)
		if m1 || m2 {
			return true
		}
	}
	return false
}

// parallelSuffix reports "N/M" with positive N <= M.
func parallelSuffix(s string) bool {
	a, b, ok := strings.Cut(s, "/")
	n, err1 := strconv.Atoi(a)
	m, err2 := strconv.Atoi(b)
	return ok && err1 == nil && err2 == nil && n >= 1 && n <= m
}
