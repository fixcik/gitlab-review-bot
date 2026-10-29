package config

import (
	"fmt"
	"strings"
	"time"
)

// Verify configures the adversarial verification of findings
// (review.verify): before publishing, the new findings of the listed
// severities go through ONE extra Claude run that tries to refute each of
// them by reading the code (confirm / downgrade / reject). The same run
// verifies Claude's breaking-change claims (review.compat_check), which is
// why Model and Timeout apply to it whether or not findings are verified.
type Verify struct {
	// Enabled turns the verification of findings on (default true). The
	// verification of breaking-change claims does not depend on it.
	Enabled bool `yaml:"enabled"`
	// Severities are the finding severities that are verified (default
	// critical, major).
	Severities []string `yaml:"severities"`
	// MaxFindings caps the findings verified per review (default 10, most
	// severe first); the rest are published unverified.
	MaxFindings int `yaml:"max_findings"`
	// Model runs the verification; empty = claude.model.
	Model string `yaml:"model"`
	// Timeout of the verification run; 0 = claude.timeout.
	Timeout Duration `yaml:"timeout"`
}

// Verify defaults and bounds.
const (
	DefaultVerifyMaxFindings = 10
	MaxVerifyMaxFindings     = 50
	maxVerifyTimeout         = 2 * time.Hour
)

func defaultVerify() Verify {
	return Verify{Enabled: true, Severities: []string{"critical", "major"}, MaxFindings: DefaultVerifyMaxFindings}
}

// SeveritySet returns the verified severities, lower-case.
func (v Verify) SeveritySet() map[string]bool {
	out := map[string]bool{}
	for _, s := range v.Severities {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out[s] = true
		}
	}
	return out
}

// Max returns max_findings (default 10).
func (v Verify) Max() int { return positiveOr(v.MaxFindings, DefaultVerifyMaxFindings) }

func (v Verify) validate() []string {
	var errs []string
	for _, s := range v.Severities {
		if _, ok := parseSeverity(s); !ok {
			errs = append(errs, fmt.Sprintf("review.verify.severities: %q is not critical, major, minor or nit", s))
		}
	}
	if v.Enabled && len(v.SeveritySet()) == 0 {
		errs = append(errs, "review.verify.severities must not be empty (use review.verify.enabled: false to turn the verification off)")
	}
	if v.MaxFindings < 1 || v.MaxFindings > MaxVerifyMaxFindings {
		errs = append(errs, fmt.Sprintf("review.verify.max_findings must be between 1 and %d", MaxVerifyMaxFindings))
	}
	if v.Timeout.D() < 0 || (v.Timeout.D() > 0 && v.Timeout.D() < time.Minute) || v.Timeout.D() > maxVerifyTimeout {
		errs = append(errs, "review.verify.timeout must be 0 (= claude.timeout) or between 1m and 2h")
	}
	return errs
}

// RepoVerify is the verify section of .reviewbot.yaml: a repository may
// only turn the verification of findings off.
type RepoVerify struct {
	Enabled *bool `yaml:"enabled"`
}

func (r *Repo) verifyEnabled() bool {
	return r == nil || r.Verify == nil || defaultTrue(r.Verify.Enabled)
}
