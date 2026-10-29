package meter

import (
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Stored run records: written by Runner (Store.RecordRun), aggregated for
// the usage page (internal/usageweb). *store.SQLite implements both sides.

// ClaudeRun is one recorded Claude run. Only metadata: never prompts, code
// or answers.
type ClaudeRun struct {
	At          time.Time
	Kind        string // review | chat | verification | learning | other
	ProjectID   int64
	ProjectPath string
	MRIID       int64
	MRTitle     string
	Username    string // "" = unattributed
	UserID      int64
	Model       string // requested model; "" = CLI default
	Usage       core.TokenUsage
	CostUSD     float64
	Duration    time.Duration
	NumTurns    int
	OK          bool
	ErrorClass  string
	// Models is the per-model split; empty = everything on Model.
	Models map[string]core.ModelUsage
}

// RunFilter selects runs for the aggregates. Zero fields match everything.
type RunFilter struct {
	Since, Until time.Time // [Since, Until)
	ProjectPath  string
	// Username, when non-nil, keeps one user's runs ("" = unattributed).
	Username *string
	// Kind keeps one run kind.
	Kind string
	// Model keeps the runs that used this model (requested or in the
	// per-model split, e.g. a subagent's model); the run counts whole.
	// HasModel says whether Model is set ("" = the CLI default model).
	Model    string
	HasModel bool
	// MRProjectPath and MRIID (> 0) keep one merge request's runs.
	MRProjectPath string
	MRIID         int64
	// Location is used to split days (default UTC).
	Location *time.Location
}

// RunStats are the sums over a set of runs.
type RunStats struct {
	Runs       int64
	Failed     int64
	Usage      core.TokenUsage
	CostUSD    float64
	DurationMS int64
}

// RunGroup is one row of an aggregate.
type RunGroup struct {
	Key string // username, project path, kind, model or day (YYYY-MM-DD)
	// ID is the user or project id for the user/project dimensions.
	ID int64
	RunStats
}

// Dimension is what RunsBy groups by.
type Dimension string

const (
	ByUser    Dimension = "user"
	ByProject Dimension = "project"
	ByKind    Dimension = "kind"
	ByModel   Dimension = "model"
	ByDay     Dimension = "day"
)

// MRStats is one merge request's consumption.
type MRStats struct {
	ProjectID   int64
	ProjectPath string
	MRIID       int64
	Title       string // latest non-empty title seen
	RunStats
}

// RunRecord is one run as the usage page's per-run rules see it (signals,
// time buckets, the previous period, filter counts). Metadata only.
type RunRecord struct {
	At          time.Time
	Kind        string
	ProjectPath string
	MRIID       int64
	MRTitle     string
	Username    string // "" = unattributed
	Model       string // requested model
	// Models are the models of the per-model split (always at least one
	// for a stored run: the requested model when there is no split).
	Models     []string
	Usage      core.TokenUsage
	CostUSD    float64
	DurationMS int64
	NumTurns   int
	OK         bool
	ErrorClass string
}

// Match reports whether r passes f's non-time conditions (the same ones
// the store applies in SQL).
func (f RunFilter) Match(r RunRecord) bool {
	if f.ProjectPath != "" && r.ProjectPath != f.ProjectPath {
		return false
	}
	if f.Username != nil && r.Username != *f.Username {
		return false
	}
	if f.Kind != "" && r.Kind != f.Kind {
		return false
	}
	if f.MRIID > 0 && (r.MRIID != f.MRIID || r.ProjectPath != f.MRProjectPath) {
		return false
	}
	if f.HasModel {
		for _, m := range r.Models {
			if m == f.Model {
				return true
			}
		}
		return false
	}
	return true
}
