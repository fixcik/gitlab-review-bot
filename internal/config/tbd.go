package config

import (
	"fmt"
	"strings"
	"time"
)

// TBD configures the trunk-based development status block of the MR
// summary (review.tbd): MR size, branch age, how far the branch is behind
// the target branch, and the head pipeline. No Claude involved.
type TBD struct {
	// Enabled shows the block (default true).
	Enabled bool `yaml:"enabled"`
	// MaxLines: changed lines (added + removed, ignored files excluded)
	// above which the MR is flagged as too large. 0 = no limit.
	MaxLines int `yaml:"max_lines"`
	// MaxAge: branch age (oldest commit's author date) above which the
	// branch is flagged as long-lived. 0 = no limit.
	MaxAge Duration `yaml:"max_age"`
	// MaxBehind: commits on the target branch missing from the source
	// branch above which a rebase is suggested. 0 = no limit.
	MaxBehind int `yaml:"max_behind"`
	// Pipeline shows the head pipeline status and keeps it fresh while the
	// pipeline runs (default true).
	Pipeline bool `yaml:"pipeline"`
	// FeatureFlags asks Claude to report (major) behavior changes of
	// functionality already in use that cannot be switched off without a
	// deploy (default true). Additive changes never need a switch.
	FeatureFlags bool `yaml:"feature_flags"`
	// FlagsHint describes the team's switch mechanism for the prompt
	// ("flags.Enabled(ctx, \"name\") из pkg/flags"); empty = any switch
	// (config key, env variable, flag) counts.
	FlagsHint string `yaml:"flags_hint"`
}

func defaultTBD() TBD {
	return TBD{Enabled: true, MaxLines: 400, MaxAge: Duration(72 * time.Hour), MaxBehind: 30, Pipeline: true, FeatureFlags: true}
}

func (t TBD) validate() []string {
	var errs []string
	if t.MaxLines < 0 {
		errs = append(errs, "review.tbd.max_lines must be >= 0")
	}
	if t.MaxAge.D() < 0 {
		errs = append(errs, "review.tbd.max_age must be >= 0")
	}
	if t.MaxBehind < 0 {
		errs = append(errs, "review.tbd.max_behind must be >= 0")
	}
	if len([]rune(t.FlagsHint)) > 1000 {
		errs = append(errs, "review.tbd.flags_hint must be at most 1000 characters")
	}
	return errs
}

// RepoTBD is the tbd section of .reviewbot.yaml: thresholds override the
// service ones in either direction (they only produce warnings).
type RepoTBD struct {
	Enabled   *bool     `yaml:"enabled"`
	MaxLines  *int      `yaml:"max_lines"`
	MaxAge    *Duration `yaml:"max_age"`
	MaxBehind *int      `yaml:"max_behind"`
	// FeatureFlags=false turns the feature-flag rule off for this repo.
	FeatureFlags *bool `yaml:"feature_flags"`
	// FlagsHint replaces review.tbd.flags_hint for this repo.
	FlagsHint string `yaml:"flags_hint"`
}

func (r *RepoTBD) normalize() error {
	if r == nil {
		return nil
	}
	if r.MaxLines != nil && *r.MaxLines < 0 {
		return fmt.Errorf("tbd.max_lines must be >= 0")
	}
	if r.MaxAge != nil && r.MaxAge.D() < 0 {
		return fmt.Errorf("tbd.max_age must be >= 0")
	}
	if r.MaxBehind != nil && *r.MaxBehind < 0 {
		return fmt.Errorf("tbd.max_behind must be >= 0")
	}
	r.FlagsHint = strings.TrimSpace(r.FlagsHint)
	if len([]rune(r.FlagsHint)) > 1000 {
		return fmt.Errorf("tbd.flags_hint must be at most 1000 characters")
	}
	return nil
}

// mergeTBD applies the repo's tbd section to the service one.
func mergeTBD(svc TBD, repo *Repo) TBD {
	t := svc
	if repo == nil || repo.TBD == nil {
		return t
	}
	r := repo.TBD
	t.Enabled = svc.Enabled && defaultTrue(r.Enabled)
	if r.MaxLines != nil {
		t.MaxLines = *r.MaxLines
	}
	if r.MaxAge != nil {
		t.MaxAge = *r.MaxAge
	}
	if r.MaxBehind != nil {
		t.MaxBehind = *r.MaxBehind
	}
	t.FeatureFlags = svc.FeatureFlags && defaultTrue(r.FeatureFlags)
	if r.FlagsHint != "" {
		t.FlagsHint = r.FlagsHint
	}
	return t
}
