// Package output holds the structured answers Claude returns to the review
// orchestrator and its feature packages: the review (ReviewOutput), a chat
// reply (ChatOutput), task alignment, the suggested title, backward
// compatibility and its verification. They are decoded from Claude's JSON
// (the schemas are in review/jsonschema) and used only under
// internal/review. Severity stays in core: config and lint use it too.
package output

import "github.com/fixcik/gitlab-review-bot/internal/core"

// Finding is one review remark from Claude.
type Finding struct {
	Path       string        `json:"path"`
	Line       int           `json:"line"`     // line in the NEW file
	EndLine    int           `json:"end_line"` // optional, >= Line
	Severity   core.Severity `json:"severity"`
	Title      string        `json:"title"`
	Body       string        `json:"body"`
	Suggestion string        `json:"suggestion,omitempty"` // replacement for lines Line..EndLine
	// DeployedUsage is the evidence of a critical finding on a database
	// migration (review.migrations): a line of the deployed code (merge
	// base) that uses the dropped or renamed object. Checked by the bot,
	// never published; nil elsewhere.
	DeployedUsage *UsageRef `json:"deployed_usage,omitempty"`
}

// UsageRef is a line of a file in some revision that uses a name.
type UsageRef struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Name string `json:"name"`
}

type FileSummary struct {
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

// ResolvedFinding says a previously open bot thread is now fixed.
type ResolvedFinding struct {
	ID      string `json:"id"` // finding ID from the prompt
	Comment string `json:"comment"`
}

// Task alignment verdicts.
const (
	VerdictMatch    = "match"
	VerdictPartial  = "partial"
	VerdictMismatch = "mismatch"
	VerdictUnclear  = "unclear"
)

// TaskAlignment is Claude's comparison of the MR with its linked Jira
// issue(s). Only requested when at least one issue was fetched.
type TaskAlignment struct {
	Verdict     string   `json:"verdict"` // match | partial | mismatch | unclear
	Implemented []string `json:"implemented"`
	Missing     []string `json:"missing"`
	Extra       []string `json:"extra"`
	Notes       string   `json:"notes"`
}

// TitleSuggestion is Claude's proposal for a Conventional Commits MR title.
// The bot appends the Jira key itself.
type TitleSuggestion struct {
	Type        string `json:"type"`
	Scope       string `json:"scope,omitempty"`
	Description string `json:"description"`
}

// Kinds of backward-incompatible changes Claude reports.
const (
	CompatKindAPI    = "api"
	CompatKindEvent  = "event"
	CompatKindDB     = "db"
	CompatKindConfig = "config"
	CompatKindOther  = "other"
)

// BreakingChange is one backward-incompatible change Claude found. Every
// reported item blocks the merge (after a separate verification run).
type BreakingChange struct {
	Path             string `json:"path"`
	Line             int    `json:"line"` // NEW file; 0 if unknown
	Kind             string `json:"kind"` // api | event | db | config | other
	Title            string `json:"title"`
	Explanation      string `json:"explanation"`
	PreviousBehavior string `json:"previous_behavior"`
	NewBehavior      string `json:"new_behavior"`
	SafeAlternative  string `json:"safe_alternative"`
}

// Compatibility is the backward-compatibility part of a review.
type Compatibility struct {
	Breaking []BreakingChange `json:"breaking"`
	// Resolved lists open breaking-change threads (reported by Claude
	// earlier) that the current code fixes.
	Resolved []ResolvedFinding `json:"resolved"`
}

// Verification verdicts for a Claude-reported breaking change.
const (
	VerdictConfirmed = "confirmed"
	VerdictRejected  = "rejected"
)

// CompatVerdict is the verification run's answer for one item.
type CompatVerdict struct {
	Index   int    `json:"index"` // 1-based number of the item in the prompt
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// CompatVerification is the output of the verification run.
type CompatVerification struct {
	Verdicts []CompatVerdict `json:"verdicts"`
}

type ReviewOutput struct {
	Summary     string            `json:"summary"`
	Walkthrough []FileSummary     `json:"walkthrough"`
	Findings    []Finding         `json:"findings"`
	Resolved    []ResolvedFinding `json:"resolved"`
	// TaskAlignment is set only when the prompt asked for it.
	TaskAlignment *TaskAlignment `json:"task_alignment,omitempty"`
	// SuggestedTitle is set only when the prompt asked for it (title not in
	// Conventional Commits format or without the Jira key).
	SuggestedTitle *TitleSuggestion `json:"suggested_title,omitempty"`
	// Compatibility is set only when the prompt asked for it (compat check
	// enabled).
	Compatibility *Compatibility `json:"compatibility,omitempty"`
	// Diagram is an optional Mermaid diagram of what the MR changes ("" =
	// none). Validated by the bot before rendering (review/crossrepo/diagram.go).
	Diagram string `json:"diagram,omitempty"`
	// DescriptionSummary is set only when the prompt asked for it
	// (review.description_summary; review/descsum).
	DescriptionSummary *DescriptionSummary `json:"description_summary,omitempty"`
}

// DescriptionSummary is Claude's summary of the whole MR for its
// description: an overview and items by group.
type DescriptionSummary struct {
	Overview string         `json:"overview"`
	Groups   []SummaryGroup `json:"groups"`
}

// SummaryGroup is one group of the description summary (kind: new, fix,
// refactor, test, docs, other).
type SummaryGroup struct {
	Kind  string   `json:"kind"`
	Items []string `json:"items"`
}

type ChatOutput struct {
	Reply string `json:"reply"`
	// Resolve: the thread is a bot finding and the discussion concluded it is
	// fixed or not an issue.
	Resolve bool `json:"resolve"`
}
