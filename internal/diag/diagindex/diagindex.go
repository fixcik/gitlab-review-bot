// Package diagindex holds the rows of the diagnostics index (internal/diag):
// one row per recorded job run, kept in SQLite (*store.SQLite) for listing
// and retention. It is a leaf, so the store implements the index without
// importing the recorder.
package diagindex

import "time"

// Row is one row of the diagnostics index.
type Row struct {
	ID          string
	StartedAt   time.Time
	FinishedAt  time.Time // zero while running
	Kind        string    // core.JobKind
	ProjectID   int64
	ProjectPath string
	MRIID       int64
	MRTitle     string
	MRURL       string
	Username    string
	Status      string // running | ok | skipped | postponed | failed | interrupted
	Reason      string
	Error       string
	RetryAt     time.Time
	Attempt     int
	DurationMS  int64
	ClaudeRuns  int
	Tokens      int64
	CostUSD     float64
	SizeBytes   int64
	Dir         string // relative to the diag root: <YYYYMMDD>/<id>
}

// Filter selects index rows. Zero fields match everything.
type Filter struct {
	Since       time.Time
	Before      time.Time
	Status      string
	Kind        string
	ProjectPath string // substring, case-insensitive
	MRIID       int64
	Limit       int  // 0 = no limit
	OldestFirst bool // default newest first
}

// Summary counts the rows of a window by status (ListDiagRuns with only
// Since would read every row to count them).
type Summary struct {
	// ByStatus: status -> rows started in the window.
	ByStatus map[string]int
	// Total rows started in the window.
	Total int
	// Last is the start of the newest row of the whole index, whatever the
	// window (zero: the index is empty).
	Last time.Time
}
