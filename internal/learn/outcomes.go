package learn

import (
	"context"
	"time"
)

// Learning from actions: what happened to each finding the bot posted as a
// thread. Repeated dismissals (a human resolved the thread without changing
// the code) are turned into rules by a light-model run.

// Outcome statuses.
const (
	// OutcomeOpen: the thread is open (or its fate is not known yet).
	OutcomeOpen = "open"
	// OutcomeFixed: the bot resolved it because the code was fixed (Claude
	// said so, or the bot asked a human to close it as fixed), or a human
	// resolved it after changing the code around the finding.
	OutcomeFixed = "fixed"
	// OutcomeDismissed: a human resolved the thread and the code around the
	// finding did not change since it was posted.
	OutcomeDismissed = "dismissed"
	// OutcomeResolvedByBot: resolved by the bot without a verdict on the
	// finding (non-blocking findings resolved right after publishing, a
	// chat answer that closed the thread).
	OutcomeResolvedByBot = "resolved_by_bot"
	// OutcomeClosed: resolved or deleted, but it could not be classified
	// (e.g. the posted head is gone after a force push). Ignored by stats
	// and learning.
	OutcomeClosed = "closed"
	// OutcomeRejectedByVerifier: the verification run (review.verify)
	// refuted the finding, so it was never posted. Recorded for the stats
	// only; no thread, never classified or used for learning.
	OutcomeRejectedByVerifier = "rejected_by_verifier"
)

// Outcome is one finding thread the bot posted.
type Outcome struct {
	ID            int64
	ProjectPath   string
	MRIID         int64
	FindingID     string
	Title         string
	Severity      string
	Path          string
	Line          int    // line in the NEW file at HeadSHAAtPost; 0 = whole file
	BodyExcerpt   string // start of the finding body (for synthesis)
	HeadSHAAtPost string
	Status        string
	DiscussionID  string // "" until bound to its GitLab discussion
	// RuleID is the rule synthesized from this (dismissed) outcome; UsedAt
	// is set once the outcome was given to a synthesis run.
	RuleID    int64
	UsedAt    time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// OutcomeFilter selects outcomes. Zero fields do not filter.
type OutcomeFilter struct {
	ProjectPath string // case-insensitive
	MRIID       int64
	Status      string
	Unused      bool // UsedAt not set
	Limit       int  // newest first; <= 0 = all
}

// DecisionFilter selects the decided outcomes of the /knowledge page
// (fixed, dismissed, rejected_by_verifier). Zero fields do not filter.
type DecisionFilter struct {
	ProjectPath string    // case-insensitive
	Since       time.Time // created_at >= Since (the window of OutcomeStats)
	Status      string    // one of the decided statuses
	Limit       int       // newest decision first; <= 0 = all
	Offset      int
}

// OutcomeStats counts outcomes by status.
type OutcomeStats struct {
	Open, Fixed, Dismissed, ResolvedByBot, Closed int
	// RejectedByVerifier: findings the verification run refuted (never
	// posted; not part of the precision).
	RejectedByVerifier int
}

// Precision is fixed/(fixed+dismissed); ok is false when both are zero.
func (s OutcomeStats) Precision() (p float64, ok bool) {
	if s.Fixed+s.Dismissed == 0 {
		return 0, false
	}
	return float64(s.Fixed) / float64(s.Fixed+s.Dismissed), true
}

// SynthRule is a rule synthesized from dismissed findings, remembered so the
// summary of the MR whose review triggered the synthesis can announce it.
type SynthRule struct {
	RuleID      int64
	ProjectPath string
	MRIID       int64
	Dismissed   int // number of dismissed findings it was learned from
	Shown       int // how many summaries announced it
	CreatedAt   time.Time
}

// OutcomeStore keeps finding outcomes. *store.SQLite implements it.
type OutcomeStore interface {
	// RecordOutcome inserts o (Status defaults to OutcomeOpen).
	RecordOutcome(ctx context.Context, o Outcome) (int64, error)
	// ListOutcomes returns matching outcomes, newest first.
	ListOutcomes(ctx context.Context, f OutcomeFilter) ([]Outcome, error)
	// SetOutcomeDiscussion binds an outcome to its GitLab discussion.
	SetOutcomeDiscussion(ctx context.Context, id int64, discussionID string) error
	// SetOutcomeStatus changes the status of an outcome.
	SetOutcomeStatus(ctx context.Context, id int64, status string) error
	// CloseOpenOutcome sets status on the OPEN outcome of a finding thread
	// of an MR (matched by discussion id, or by finding id while unbound).
	// It reports whether an outcome was updated.
	CloseOpenOutcome(ctx context.Context, projectPath string, mrIID int64, discussionID, findingID, status string) (bool, error)
	// TouchOutcomes bumps updated_at of the MR's open outcomes (they were
	// looked at; used to rotate which closed MRs are checked).
	TouchOutcomes(ctx context.Context, projectPath string, mrIID int64) error
	// MarkOutcomesUsed records that the outcomes went into a synthesis run
	// (ruleID may be 0 when nothing was learned).
	MarkOutcomesUsed(ctx context.Context, ids []int64, ruleID int64) error
	// OutcomeStats counts the project's outcomes created at or after since.
	OutcomeStats(ctx context.Context, projectPath string, since time.Time) (OutcomeStats, error)
	// AddSynthRule remembers a synthesized rule for announcing.
	AddSynthRule(ctx context.Context, s SynthRule) error
	// PendingSynthRules returns synthesized rules of an MR announced fewer
	// than maxShown times, oldest first.
	PendingSynthRules(ctx context.Context, projectPath string, mrIID int64, maxShown int) ([]SynthRule, error)
	// MarkSynthRulesShown increments the shown counter of the rules.
	MarkSynthRulesShown(ctx context.Context, ruleIDs []int64) error
	// SynthRuleSources maps rule ids to the number of dismissed findings
	// they were synthesized from (rules not synthesized are absent).
	SynthRuleSources(ctx context.Context, ruleIDs []int64) (map[int64]int, error)
}
