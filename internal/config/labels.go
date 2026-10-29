package config

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/goccy/go-yaml"
)

// Labels configures automatic MR labels (review.labels, .reviewbot.yaml
// labels): after every review the bot sets the labels whose rules apply and
// (remove_stale) takes off the labels it set itself earlier that no longer
// apply. Labels added by people are never removed. See
// docs/features/labels.md.
type Labels struct {
	// Enabled is the service switch (default true): with false no
	// repository gets labels. Without rules nothing happens either way.
	Enabled bool `yaml:"enabled"`
	// RemoveStale: remove labels the bot added earlier when their rule no
	// longer applies (default true).
	RemoveStale bool `yaml:"remove_stale"`
	// CreateMissing: a configured label that does not exist in the project
	// or its groups is created by GitLab when the bot sets it (true), or
	// skipped and reported as a verification problem (false, default).
	CreateMissing bool `yaml:"create_missing"`
	// Rules: at most MaxLabelRules.
	Rules []LabelRule `yaml:"rules"`
}

// LabelRule is one label and when it applies. A rule applies when any of
// its deterministic conditions matches (Paths, WhenFact) or the review
// model says its Instruction holds.
type LabelRule struct {
	// Name is the GitLab label ("backend", "type::bug").
	Name string `yaml:"name"`
	// Paths: doublestar globs; the label applies when any file changed by
	// the MR matches one of them.
	Paths []string `yaml:"paths"`
	// WhenFact: deterministic facts the bot computes itself (LabelFacts);
	// one name or a list, any of them matches.
	WhenFact FactList `yaml:"when_fact"`
	// Instruction: when the label applies, in words; the review model
	// decides.
	Instruction string `yaml:"instruction"`
	// Enabled=false removes a service rule of the same name in a
	// repository (default true).
	Enabled *bool `yaml:"enabled"`
}

// FactList is one fact name or a list of them.
type FactList []string

// UnmarshalYAML accepts a scalar ("migrations") or a sequence.
func (f *FactList) UnmarshalYAML(b []byte) error {
	var list []string
	if err := yaml.Unmarshal(b, &list); err == nil {
		*f = list
		return nil
	}
	var one string
	if err := yaml.Unmarshal(b, &one); err != nil {
		return fmt.Errorf("when_fact must be a fact name or a list of names: %w", err)
	}
	if strings.TrimSpace(one) == "" {
		*f = nil
		return nil
	}
	*f = FactList{one}
	return nil
}

// Deterministic facts a label rule may use (when_fact).
const (
	// FactMigrations: the MR adds or changes database migrations
	// (review.migrations detection).
	FactMigrations = "migrations"
	// FactAPIChanged: the MR changes an API spec (OpenAPI, proto, GraphQL;
	// with the compat check off only OpenAPI changelog items count).
	FactAPIChanged = "api_changed"
	// FactAPIBreaking: a deterministic checker (oasdiff, buf,
	// graphql-inspector) found a breaking change.
	FactAPIBreaking = "api_breaking"
	// FactBigMR: the MR is larger than review.tbd.max_lines (the size
	// warning of the trunk-based block).
	FactBigMR = "big_mr"
	// FactLintSecret: gitleaks found a leaked secret in the MR.
	FactLintSecret = "lint_secret"
	// FactNoTestsChanged: the MR changes code files but no test files.
	FactNoTestsChanged = "no_tests_changed"
)

// LabelFacts lists every fact name, in documentation order.
func LabelFacts() []string {
	return []string{FactMigrations, FactAPIChanged, FactAPIBreaking, FactBigMR, FactLintSecret, FactNoTestsChanged}
}

// Limits of labels.
const (
	MaxLabelRules            = 30
	MaxLabelNameRunes        = 255
	MaxLabelInstructionRunes = 1000
	MaxLabelPaths            = 50
)

func defaultLabels() Labels { return Labels{Enabled: true, RemoveStale: true} }

// ValidLabelName reports whether s can be sent as a GitLab label: non-empty,
// at most 255 characters, no commas (the API takes comma-separated lists)
// and no control characters.
func ValidLabelName(s string) bool {
	if strings.TrimSpace(s) == "" || len([]rune(s)) > MaxLabelNameRunes || strings.Contains(s, ",") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// LabelScope is the scope of a scoped label ("type" for "type::bug",
// "a::b" for "a::b::c": the part before the last "::"), "" otherwise.
func LabelScope(name string) string {
	i := strings.LastIndex(name, "::")
	if i <= 0 || i+2 >= len(name) {
		return ""
	}
	return name[:i]
}

// Deterministic reports whether the rule has a condition the bot checks
// itself (paths or facts).
func (r LabelRule) Deterministic() bool { return len(r.Paths) > 0 || len(r.WhenFact) > 0 }

// On reports whether the rule is active (enabled is not false).
func (r LabelRule) On() bool { return defaultTrue(r.Enabled) }

// MatchFile reports whether file matches one of the rule's path globs.
func (r LabelRule) MatchFile(file string) bool {
	for _, g := range r.Paths {
		if ok, err := doublestar.Match(g, file); err == nil && ok {
			return true
		}
	}
	return false
}

// normalizeLabelRules validates and cleans a rules list; where names it in
// errors ("labels.rules", "review.labels.rules"). Disabled rules
// (enabled: false) only need a valid name.
func normalizeLabelRules(list []LabelRule, where string) ([]LabelRule, error) {
	if len(list) > MaxLabelRules {
		return nil, fmt.Errorf("%s: at most %d rules allowed, got %d", where, MaxLabelRules, len(list))
	}
	out := make([]LabelRule, 0, len(list))
	seen := map[string]bool{}
	for i, r := range list {
		name := strings.TrimSpace(r.Name)
		if !ValidLabelName(name) {
			return nil, fmt.Errorf("%s[%d]: invalid label name %q (non-empty, at most %d characters, no commas or control characters)", where, i, r.Name, MaxLabelNameRunes)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s[%d]: duplicate label %q", where, i, name)
		}
		seen[name] = true
		c := LabelRule{Name: name, Enabled: r.Enabled, Instruction: strings.TrimSpace(r.Instruction)}
		if len(r.Paths) > MaxLabelPaths {
			return nil, fmt.Errorf("%s[%d] (%s): at most %d paths", where, i, name, MaxLabelPaths)
		}
		for _, p := range r.Paths {
			g := cleanGlob(p)
			if g == "" || !doublestar.ValidatePattern(g) {
				return nil, fmt.Errorf("%s[%d] (%s): invalid glob %q", where, i, name, p)
			}
			c.Paths = append(c.Paths, g)
		}
		for _, f := range r.WhenFact {
			f = strings.ToLower(strings.TrimSpace(f))
			if !slices.Contains(LabelFacts(), f) {
				return nil, fmt.Errorf("%s[%d] (%s): unknown when_fact %q (known: %s)", where, i, name, f, strings.Join(LabelFacts(), ", "))
			}
			c.WhenFact = append(c.WhenFact, f)
		}
		if n := len([]rune(c.Instruction)); n > MaxLabelInstructionRunes {
			return nil, fmt.Errorf("%s[%d] (%s): instruction longer than %d characters", where, i, name, MaxLabelInstructionRunes)
		}
		if c.On() && !c.Deterministic() && c.Instruction == "" {
			return nil, fmt.Errorf("%s[%d] (%s): a rule needs paths, when_fact or instruction", where, i, name)
		}
		out = append(out, c)
	}
	return out, nil
}

func (l Labels) validate(overrideLabel string) []string {
	rules, err := normalizeLabelRules(l.Rules, "review.labels.rules")
	if err != nil {
		return []string{err.Error()}
	}
	for i, r := range rules {
		if r.On() && IsOverrideLabel(r.Name, overrideLabel) {
			return []string{fmt.Sprintf("review.labels.rules[%d] (%s): the bot must not set review.big_mr.override_label: that would lift the size limit and big_mr.block by itself", i, r.Name)}
		}
	}
	return nil
}

// IsOverrideLabel reports whether label is the size-override label
// (review.big_mr.override_label; GitLab label names compare case-insensitively).
func IsOverrideLabel(label, overrideLabel string) bool {
	return overrideLabel != "" && strings.EqualFold(strings.TrimSpace(label), overrideLabel)
}

// RepoLabels is the labels section of .reviewbot.yaml. Rules extend the
// service rules; a rule with the name of a service rule replaces it
// (enabled: false removes it).
type RepoLabels struct {
	// Enabled=false turns labels off for the repository (it cannot turn
	// them on when the service switch is off).
	Enabled       *bool       `yaml:"enabled"`
	RemoveStale   *bool       `yaml:"remove_stale"`
	CreateMissing *bool       `yaml:"create_missing"`
	Rules         []LabelRule `yaml:"rules"`
}

func (r *RepoLabels) normalize() error {
	if r == nil {
		return nil
	}
	rules, err := normalizeLabelRules(r.Rules, "labels.rules")
	if err != nil {
		return err
	}
	r.Rules = rules
	return nil
}

// EffLabels is the effective label configuration of one repository.
type EffLabels struct {
	// Enabled: review.labels.enabled AND labels.enabled of the repo.
	Enabled       bool
	RemoveStale   bool
	CreateMissing bool
	// Rules: service rules in order (a repo rule of the same name replaces
	// one in place), then the repo's new rules; disabled ones dropped; at
	// most MaxLabelRules.
	Rules []LabelRule
	// Dropped counts repo rules beyond MaxLabelRules (ignored; logged).
	Dropped int
	// Refused: rules named like review.big_mr.override_label (any case).
	// Never applied: the bot setting that label would lift the size limit
	// and big_mr.block by itself. The service config refuses such a rule
	// at startup; a repository's is only known at merge time (logged).
	Refused []string
}

// On reports whether there is anything to do.
func (e EffLabels) On() bool { return e.Enabled && len(e.Rules) > 0 }

// InstructionRules returns the rules the model decides on (with an
// instruction), in order.
func (e EffLabels) InstructionRules() []LabelRule {
	var out []LabelRule
	for _, r := range e.Rules {
		if r.Instruction != "" {
			out = append(out, r)
		}
	}
	return out
}

// mergeLabels: invalid service rules are dropped (Validate reports them at
// startup); repo rules were validated by ParseRepo. repo may be nil. A rule
// for the size-override label (overrideLabel) is refused (EffLabels.Refused).
func mergeLabels(svc Labels, repo *Repo, overrideLabel string) EffLabels {
	e := EffLabels{Enabled: svc.Enabled, RemoveStale: svc.RemoveStale, CreateMissing: svc.CreateMissing}
	rules, err := normalizeLabelRules(svc.Rules, "review.labels.rules")
	if err != nil {
		rules = nil
	}
	if repo != nil && repo.Labels != nil {
		rl := repo.Labels
		e.Enabled = e.Enabled && defaultTrue(rl.Enabled)
		if rl.RemoveStale != nil {
			e.RemoveStale = *rl.RemoveStale
		}
		if rl.CreateMissing != nil {
			e.CreateMissing = *rl.CreateMissing
		}
		for _, r := range rl.Rules {
			replaced := false
			for i := range rules {
				if rules[i].Name == r.Name {
					rules[i], replaced = r, true
					break
				}
			}
			if !replaced {
				rules = append(rules, r)
			}
		}
	}
	for _, r := range rules {
		if !r.On() {
			continue
		}
		if IsOverrideLabel(r.Name, overrideLabel) {
			e.Refused = append(e.Refused, r.Name)
			continue
		}
		if len(e.Rules) == MaxLabelRules {
			e.Dropped++
			continue
		}
		e.Rules = append(e.Rules, r)
	}
	return e
}
