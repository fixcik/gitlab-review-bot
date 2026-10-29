// Package diag keeps enough information to diagnose the bot's recent runs
// (48 hours by default): one record per handled queue job with its timeline
// (GitLab calls, checkout, exec commands, limiter decisions, log
// lines of the job) and every Claude run in full (prompts, schema, argv,
// stream-json transcript, output, usage, stderr).
//
// The Recorder travels in the context, like internal/meter's attribution:
// Handler wraps the queue handler and starts a Run per job; decorators of
// core.Runner, core.GitLab, core.Limiter and core.Repos add steps to the Run
// found in the context; LogHandler attaches the job's log lines. A nil
// *Recorder (diagnostics disabled) makes every hook a no-op.
//
// The same hooks feed tracing (trace.go): every step becomes a span of the
// job's trace when the job is traced, with or without a Recorder (see
// Hooks).
//
// Every stored string goes through the scrubber (known secret values, token
// formats, URLs with credentials, key=value secrets). Code and prompts are
// stored as they are.
//
// On disk: <root>/<YYYYMMDD>/<run id>/run.json and claude-<n>.jsonl, plus an
// index row in SQLite (IndexRow) for listing and retention.
package diag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Run statuses.
const (
	StatusRunning     = "running"
	StatusOK          = "ok"
	StatusSkipped     = "skipped"
	StatusPostponed   = "postponed"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

// Step statuses.
const (
	StepOK      = "ok"
	StepError   = "error"
	StepDenied  = "denied"
	StepWarn    = "warn"
	StepInfo    = "info"
	StepRunning = "running"
)

// Step kinds.
const (
	KindQueue    = "queue"
	KindGitLab   = "gitlab"
	KindCheckout = "checkout"
	KindGit      = "git"
	KindExec     = "exec"
	KindClaude   = "claude"
	KindLimiter  = "limiter"
	KindLog      = "log"
	KindStep     = "step"
)

// Defaults.
const (
	DefaultRetention     = 48 * time.Hour
	DefaultMaxRunBytes   = 20 << 20
	DefaultMaxTotalBytes = 2048 << 20
	// TailBytes caps stored stdout/stderr per command.
	TailBytes = 8 << 10
	// writeEvery throttles run.json rewrites while a job runs.
	writeEvery = 2 * time.Second
)

// Fields are extra key/values of a step.
type Fields map[string]any

// Index is the SQLite index of runs (*store.SQLite).
type Index interface {
	SaveDiagRun(ctx context.Context, r IndexRow) error
	GetDiagRun(ctx context.Context, id string) (*IndexRow, error)
	ListDiagRuns(ctx context.Context, f IndexFilter) ([]IndexRow, error)
	DeleteDiagRun(ctx context.Context, id string) error
	DiagTotalSize(ctx context.Context) (int64, error)
	MarkDiagRunsInterrupted(ctx context.Context) (int64, error)
}

// Options configure a Recorder.
type Options struct {
	Root          string // <data_dir>/diag
	Index         Index
	Retention     time.Duration
	MaxRunBytes   int64
	MaxTotalBytes int64
	// Secrets are literal values that must never be stored (tokens from the
	// config and the environment). Values shorter than 8 bytes are ignored.
	Secrets []string
	// MaxDiffBytes is review.max_diff_bytes, to note when a diff is capped.
	MaxDiffBytes int
	// GitLabURL builds MR links when the MR could not be read.
	GitLabURL string
	Now       func() time.Time
	Logger    *slog.Logger
}

// Recorder writes run records. A nil *Recorder records nothing.
type Recorder struct {
	o     Options
	scrub *scrubber
	log   *slog.Logger

	mu       sync.Mutex
	active   map[string]*Run // by MR key, for the log tap
	attempts map[string]attempt
}

type attempt struct {
	n    int
	last time.Time
}

// New validates options and creates the root directory.
func New(o Options) (*Recorder, error) {
	if o.Root == "" || o.Index == nil {
		return nil, errors.New("diag: Root and Index are required")
	}
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.MaxRunBytes <= 0 {
		o.MaxRunBytes = DefaultMaxRunBytes
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	o.GitLabURL = strings.TrimRight(o.GitLabURL, "/")
	if err := os.MkdirAll(o.Root, 0o750); err != nil {
		return nil, fmt.Errorf("diag: %w", err)
	}
	rec := &Recorder{o: o, scrub: newScrubber(o.Secrets), log: o.Logger,
		active: map[string]*Run{}, attempts: map[string]attempt{}}
	rec.markInterrupted()
	return rec, nil
}

// Root is the directory holding the run directories.
func (rec *Recorder) Root() string {
	if rec == nil {
		return ""
	}
	return rec.o.Root
}
