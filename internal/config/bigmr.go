package config

import (
	"fmt"
	"strings"
)

// Size policy of the review (review.max_mr_lines, review.big_mr): an MR
// (or, on an incremental review, the increment) with more changed lines
// than MaxMRLines gets no Claude run at all, only a summary note and the
// cheap deterministic checks. See docs/features/review.md.

// DefaultMaxMRLines is the default review.max_mr_lines.
const DefaultMaxMRLines = 2000

// DefaultBigMROverrideLabel is the default review.big_mr.override_label.
const DefaultBigMROverrideLabel = "big-mr-ok"

// BigMR configures what happens to an MR over review.max_mr_lines.
type BigMR struct {
	// Deterministic runs the cheap deterministic checks without Claude on a
	// too-big MR: gitleaks (secrets) and the deterministic compat checkers
	// (oasdiff, buf, graphql-inspector). Default true.
	Deterministic bool `yaml:"deterministic"`
	// OverrideLabel: an MR with this label is reviewed whatever its size.
	// Empty = no label override (the "review force" command still works).
	OverrideLabel string `yaml:"override_label"`
	// Block makes a too-big MR merge-blocking: the bot opens a blocking
	// thread and resolves it when the MR shrinks below the threshold or the
	// threshold is overridden. Default false (informational).
	Block bool `yaml:"block"`
}

func defaultBigMR() BigMR {
	return BigMR{Deterministic: true, OverrideLabel: DefaultBigMROverrideLabel}
}

func (r Review) validateBigMR() []string {
	var errs []string
	if r.MaxMRLines < 0 {
		errs = append(errs, "review.max_mr_lines must be >= 0")
	}
	if l := r.BigMR.OverrideLabel; strings.TrimSpace(l) != l || strings.Contains(l, ",") || len([]rune(l)) > 255 {
		errs = append(errs, "review.big_mr.override_label must be a label name (no surrounding spaces, no commas, at most 255 characters)")
	}
	return errs
}

// RepoBigMR is the big_mr section of .reviewbot.yaml. It can only make the
// policy stricter.
type RepoBigMR struct {
	// Block=true makes a too-big MR merge-blocking in this repository
	// (false does not turn the service setting off).
	Block *bool `yaml:"block"`
}

func (r *Repo) normalizeBigMR() error {
	if r.MaxMRLines < 0 {
		return fmt.Errorf("max_mr_lines must be >= 0")
	}
	return nil
}

// EffBigMR is the effective size policy for one repository.
type EffBigMR struct {
	// MaxLines: changed lines above which Claude does not run; 0 = off.
	MaxLines      int
	Deterministic bool
	OverrideLabel string
	Block         bool
}

// On reports whether the size limit applies.
func (b EffBigMR) On() bool { return b.MaxLines > 0 }

// mergeBigMR: the repository's max_mr_lines applies only when it is
// stricter (lower, or a limit where the service has none); big_mr.block can
// only be turned on.
func mergeBigMR(svc Review, repo *Repo) EffBigMR {
	e := EffBigMR{MaxLines: svc.MaxMRLines, Deterministic: svc.BigMR.Deterministic,
		OverrideLabel: svc.BigMR.OverrideLabel, Block: svc.BigMR.Block}
	if repo == nil {
		return e
	}
	e.MaxLines = stricter(e.MaxLines, repo.MaxMRLines)
	if repo.BigMR != nil {
		e.Block = e.Block || setTrue(repo.BigMR.Block)
	}
	return e
}
