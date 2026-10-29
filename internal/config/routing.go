package config

import (
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Model routing by MR risk (review.routing): a low-risk review range (few
// changed lines, only docs / config / test files, no risk signal) is
// reviewed by the light model, without the advisor; everything else by
// claude.model as before. See docs/features/review.md.

// DefaultRoutingMaxLines is the default review.routing.max_lines.
const DefaultRoutingMaxLines = 60

// DefaultRoutingHeavyMinLines is the default review.routing.heavy_min_lines.
const DefaultRoutingHeavyMinLines = 300

// DefaultRoutingLowRiskPaths are the default review.routing.low_risk_paths:
// documentation, tests and harmless repository configuration. Not "*.txt":
// requirements*.txt, constraints.txt or CMakeLists.txt are build input, not
// documentation; not .gitattributes: it changes what a diff shows.
var DefaultRoutingLowRiskPaths = []string{
	// Documentation.
	"**/*.md", "**/*.markdown", "**/*.rst", "**/*.adoc", "**/README*.txt", "docs/**", "doc/**",
	"**/LICENSE*", "**/CHANGELOG*", "**/AUTHORS*", "**/CODEOWNERS",
	// Tests.
	"**/*_test.go", "**/test_*.py", "**/*_test.py", "**/tests/**", "**/__tests__/**", "**/testdata/**",
	"**/*.test.ts", "**/*.test.tsx", "**/*.test.js", "**/*.test.jsx", "**/*.spec.ts", "**/*.spec.tsx",
	"**/*.spec.js", "**/*.spec.jsx", "**/*Test.java", "**/*Test.kt", "**/*Tests.cs", "**/*_spec.rb",
	// Repository configuration without effect on the build or the runtime.
	"**/.gitignore", "**/.editorconfig", "**/.prettierrc*", "**/.eslintrc*",
	"**/.markdownlint*", "**/.dockerignore",
}

// DefaultRoutingSensitivePaths are the default review.routing.sensitive_paths:
// a changed file matching one of them always gets the normal model. The
// dependency manifests and lock files are here so that one under a
// low-risk directory (docs/requirements.txt, tests/package.json) is not
// reviewed by the light model.
var DefaultRoutingSensitivePaths = []string{
	"**/auth/**", "**/security/**", "**/*crypto*", "**/*crypto*/**", "**/payment*/**", "**/billing/**",
	"**/Dockerfile*", "**/.gitlab-ci.yml", ".gitlab/**", "**/migrations/**", "**/CMakeLists.txt",
	// Dependency manifests and lock files.
	"**/requirements*.txt", "**/requirements/**", "**/constraints*.txt", "**/Pipfile", "**/Pipfile.lock",
	"**/pyproject.toml", "**/setup.py", "**/setup.cfg", "**/poetry.lock", "**/uv.lock",
	"**/package.json", "**/package-lock.json", "**/npm-shrinkwrap.json", "**/yarn.lock", "**/pnpm-lock.yaml",
	"**/go.mod", "**/go.sum", "**/Gemfile", "**/Gemfile.lock", "**/Cargo.toml", "**/Cargo.lock",
	"**/pom.xml", "**/build.gradle*", "**/settings.gradle*", "**/composer.json", "**/composer.lock",
}

// Routing configures review.routing.
type Routing struct {
	// Enabled routes low-risk reviews to LightModel (default true).
	Enabled bool `yaml:"enabled"`
	// LightModel reviews low-risk ranges; empty = claude.light_model.
	LightModel string `yaml:"light_model"`
	// MaxLines: a low-risk range has at most this many changed lines
	// (added + removed, ignored files excluded). Default 60.
	MaxLines int `yaml:"max_lines"`
	// LowRiskPaths: every reviewed file must match one of these globs.
	LowRiskPaths []string `yaml:"low_risk_paths"`
	// SensitivePaths: a reviewed file matching one of these globs is a
	// risk signal (normal model), even when it matches LowRiskPaths.
	SensitivePaths []string `yaml:"sensitive_paths"`
	// HeavyModel reviews heavy ranges (HeavyMinLines, HeavyPaths) without
	// the advisor and the deep-review subagent; empty = no heavy route
	// (default). Independent of Enabled (which is about the light route).
	HeavyModel string `yaml:"heavy_model"`
	// HeavyMinLines: a range with at least this many changed lines (added +
	// removed, ignored files excluded) is heavy. Default 300.
	HeavyMinLines int `yaml:"heavy_min_lines"`
	// HeavyPaths: a reviewed file (old paths of renames included) matching
	// one of these globs makes the range heavy. Default none.
	HeavyPaths []string `yaml:"heavy_paths"`
}

func defaultRouting() Routing {
	return Routing{
		Enabled: true, MaxLines: DefaultRoutingMaxLines, HeavyMinLines: DefaultRoutingHeavyMinLines,
		LowRiskPaths:   append([]string(nil), DefaultRoutingLowRiskPaths...),
		SensitivePaths: append([]string(nil), DefaultRoutingSensitivePaths...),
	}
}

func (r Routing) validate() []string {
	var errs []string
	if r.MaxLines < 1 {
		errs = append(errs, "review.routing.max_lines must be >= 1")
	}
	errs = append(errs, validateGlobs("review.routing.low_risk_paths", normalizeGlobs(r.LowRiskPaths))...)
	errs = append(errs, validateGlobs("review.routing.sensitive_paths", normalizeGlobs(r.SensitivePaths))...)
	if m := strings.TrimSpace(r.HeavyModel); m != "" && !modelNameRe.MatchString(m) {
		errs = append(errs, "review.routing.heavy_model must be a model name or alias (letters, digits, . _ : [ ] -)")
	}
	if r.HeavyMinLines < 1 {
		errs = append(errs, "review.routing.heavy_min_lines must be >= 1")
	}
	heavyPaths := normalizeGlobs(r.HeavyPaths)
	errs = append(errs, validateGlobs("review.routing.heavy_paths", heavyPaths)...)
	if len(heavyPaths) > 0 && strings.TrimSpace(r.HeavyModel) == "" {
		errs = append(errs, "review.routing.heavy_paths needs review.routing.heavy_model (the heavy route is off without it)")
	}
	return errs
}

// EffRouting is the effective routing of one repository.
type EffRouting struct {
	Enabled        bool
	LightModel     string
	MaxLines       int
	LowRiskPaths   []string
	SensitivePaths []string
	// HeavyModel "" = no heavy route.
	HeavyModel    string
	HeavyMinLines int
	HeavyPaths    []string
}

// ModelRank orders model names by capability: haiku 1, sonnet 2, opus 3,
// 0 = unknown (aliases and full ids both work: "claude-sonnet-4-5").
func ModelRank(m string) int {
	m = strings.ToLower(m)
	switch {
	case strings.Contains(m, "opus"):
		return 3
	case strings.Contains(m, "sonnet"):
		return 2
	case strings.Contains(m, "haiku"):
		return 1
	}
	return 0
}

// mergeRouting is the service's routing: models are chosen only in the
// service config (a repository's .reviewbot.yaml has no say, see
// RemovedRepoModelReason). With no light model at all (both
// review.routing.light_model and claude.light_model empty) routing is off.
func mergeRouting(svc *Service, _ *Repo) EffRouting {
	r := svc.Review.Routing
	e := EffRouting{Enabled: r.Enabled, LightModel: strings.TrimSpace(r.LightModel), MaxLines: r.MaxLines,
		LowRiskPaths: normalizeGlobs(r.LowRiskPaths), SensitivePaths: normalizeGlobs(r.SensitivePaths),
		HeavyModel: strings.TrimSpace(r.HeavyModel), HeavyMinLines: r.HeavyMinLines, HeavyPaths: normalizeGlobs(r.HeavyPaths)}
	if e.HeavyMinLines < 1 {
		e.HeavyModel = ""
	}
	if e.LightModel == "" {
		e.LightModel = strings.TrimSpace(svc.Claude.LightModel)
	}
	if e.LightModel == "" || e.MaxLines < 1 {
		e.Enabled = false
	}
	return e
}

// MatchAnyPath reports whether path matches one of the doublestar globs.
func MatchAnyPath(globs []string, path string) bool {
	for _, g := range globs {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		if ok, err := doublestar.Match(g, path); err == nil && ok {
			return true
		}
	}
	return false
}
