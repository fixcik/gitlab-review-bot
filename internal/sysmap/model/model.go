// Package model holds the stored records of the system map (internal/sysmap)
// and the Store interface that persists them (*store.SQLite). It is a leaf,
// so the store implements it without importing the map's extraction and
// matching code.
package model

import (
	"context"
	"time"
)

// Side says whether a repository provides or consumes a contract.
type Side string

// Sides.
const (
	// Provides: serves an HTTP endpoint, receives webhooks at an endpoint,
	// is the server of a WebSocket/SSE endpoint or emits its events.
	Provides Side = "provides"
	// Consumes: calls an HTTP endpoint, sends a webhook, connects to a
	// WebSocket/SSE endpoint or listens to its events.
	Consumes Side = "consumes"
)

// Protocol of a contract.
type Protocol string

// Protocols.
const (
	HTTP    Protocol = "http"
	Webhook Protocol = "webhook"
	WS      Protocol = "ws"
	SSE     Protocol = "sse"
)

// Sources of an entry.
const (
	SourceOpenAPI = "openapi"
	SourceCode    = "code"
)

// Entry is one contract usage found in a repository file.
type Entry struct {
	Side     Side     `json:"side"`
	Protocol Protocol `json:"protocol"`
	// Method is the HTTP method (upper case; "" = unknown / any).
	Method string `json:"method,omitempty"`
	// Path is the normalized path template ("/orders/{id}"), "" for
	// event-only entries.
	Path string `json:"path,omitempty"`
	// Event is the event / message name (ws, sse, webhook event).
	Event string `json:"event,omitempty"`
	// Raw is the expression as written in the code, e.g.
	// `${ORDERS_URL}/orders/${id}` (untrusted, shown escaped).
	Raw string `json:"raw,omitempty"`
	// Base is the base URL hint: an env var or host expression
	// ("ORDERS_URL", "config('services.orders.url')").
	Base    string `json:"base,omitempty"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line,omitempty"`
	Note    string `json:"note,omitempty"`
	// Source: SourceOpenAPI or SourceCode.
	Source string `json:"source"`
	// ExternalHost (consumers only): the host of a third-party system the
	// call goes to ("oauth.yandex.ru"), as the model marked it and
	// sysmap.NormalizeHost accepted it; "" for calls that may reach a
	// service of the team. Records stored before the field have it empty.
	ExternalHost string `json:"external_host,omitempty"`
	// ExternalService is a short name of that system ("Yandex OAuth"),
	// set only together with ExternalHost.
	ExternalService string `json:"external_service,omitempty"`
}

// FileRecord is the stored extraction result of one file.
type FileRecord struct {
	Path string
	// Hash is the sha256 of the content (hex).
	Hash string
	// Version is CodeVersion or SpecVersion at extraction time.
	Version string
	Entries []Entry
	// Error is set when the model's answer for the file's batch could not
	// be used; the file is not retried until its content changes.
	Error       string
	ExtractedAt time.Time
}

// Repository scan statuses.
const (
	StatusOK      = "ok"      // every candidate file is up to date at SHA
	StatusPartial = "partial" // some files wait for the run budget
	StatusError   = "error"   // the last pass failed for this repository
)

// RepoState is the scan state of one repository.
type RepoState struct {
	Path   string
	SHA    string // default-branch SHA of the last pass
	Branch string
	// ScannedAt is the time of the last pass that looked at the repository.
	ScannedAt time.Time
	Status    string
	Error     string
	// Candidates: code files selected for extraction; Specs: OpenAPI files.
	Candidates int
	Specs      int
	// Pending: candidate files still waiting for extraction.
	Pending int
	// Capped: candidate files dropped by max_files_per_repo; TooLarge:
	// files skipped by max_file_kb.
	Capped   int
	TooLarge int
	// Version: the extractor versions of a complete pass (see
	// Map.version); a change re-plans the repository.
	Version string
}

// RunRecord is one extraction run (one batch) of the light model.
type RunRecord struct {
	At       time.Time
	Repo     string
	Files    int
	OK       bool
	Error    string
	CostUSD  float64
	Tokens   int64
	Duration time.Duration
	// Kind is the budget the run counts toward: RunKindScan (the map of
	// main branches) or RunKindDeps (phase B, files of a merge request).
	Kind string
}

// Run kinds: each has its own hourly budget.
const (
	RunKindScan = ""
	RunKindDeps = "deps"
)

// RunStats aggregates RunRecords.
type RunStats struct {
	Runs, Failed int
	CostUSD      float64
	Tokens       int64
}

// Store persists the map (implemented by *store.SQLite).
type Store interface {
	ListSysmapRepos(ctx context.Context) ([]RepoState, error)
	SaveSysmapRepo(ctx context.Context, s RepoState) error
	DeleteSysmapRepo(ctx context.Context, repo string) error
	// ListSysmapFiles returns the files of one repository.
	ListSysmapFiles(ctx context.Context, repo string) ([]FileRecord, error)
	// ListAllSysmapFiles returns the files of every repository.
	ListAllSysmapFiles(ctx context.Context) (map[string][]FileRecord, error)
	SaveSysmapFiles(ctx context.Context, repo string, files []FileRecord) error
	DeleteSysmapFiles(ctx context.Context, repo string, paths []string) error
	RecordSysmapRun(ctx context.Context, r RunRecord) error
	SysmapRunStats(ctx context.Context, since time.Time) (RunStats, error)
	// SysmapRunCount counts the runs of one kind since t (budgets).
	SysmapRunCount(ctx context.Context, since time.Time, kind string) (int, error)
	// SysmapEntriesByHash returns code extraction results of an extractor
	// version by content hash: default-branch files of any repository and
	// the merge-request cache (Path empty).
	SysmapEntriesByHash(ctx context.Context, version string, hashes []string) (map[string]FileRecord, error)
	// SaveSysmapCache stores merge-request extraction results by hash.
	SaveSysmapCache(ctx context.Context, recs []FileRecord) error
}
