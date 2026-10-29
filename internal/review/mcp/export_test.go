package mcp

import "github.com/fixcik/gitlab-review-bot/internal/review/output"

// Aliases for the package's external tests (package mcp_test).

// Tool names.
const (
	ToolDiffLines  = toolDiffLines
	ToolAPIChanges = toolAPIChanges
	ToolLint       = toolLint
	ToolCoverage   = toolCoverage
	ToolMapLookup  = toolMapLookup
	ToolMigrations = toolMigrations
	ToolTask       = toolTask
	ToolRelated    = toolRelated
	ToolSubmit     = toolSubmit
	ToolWithdraw   = toolWithdraw
	ToolList       = toolList
	ToolProgress   = toolProgress
)

// The answer of submit_finding.
type Verdict = verdict

const (
	VerdictAccepted = verdictAccepted
	VerdictAdjusted = verdictAdjusted
	VerdictRejected = verdictRejected
)

// Fallback reasons.
const (
	FallbackDropped      = fallbackDropped
	FallbackNotConnected = fallbackNotConnected
	FallbackNoRequest    = fallbackNoRequest
)

// Findings are the accepted findings in submission order.
func (m *Run) Findings() []output.Finding { return m.findings() }

// SetIdxOK marks the MR diff as loaded or not (false: as if GetMRDiffs
// failed).
func (m *Run) SetIdxOK(ok bool) { m.idxOK = ok }
