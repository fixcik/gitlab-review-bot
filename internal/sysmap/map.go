package sysmap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Snapshots provides default-branch snapshots of the group repositories
// (implemented by *contextrepos.Provider).
type Snapshots interface {
	Acquire(ctx context.Context, req core.ContextRequest) ([]core.ContextDir, []core.ContextMissing, func())
}

// Defaults of Options.
const (
	DefaultInterval        = 10 * time.Minute
	DefaultMaxFilesPerRepo = 400
	DefaultMaxFileBytes    = 64 << 10
	defaultRunTimeout      = 5 * time.Minute
	defaultInitialDelay    = time.Minute
	// staleAfter: a repository missing from every pass for this long (deleted,
	// archived, moved out of the groups) is dropped from the map.
	staleAfter = 24 * time.Hour
)

// Options configure a Map.
type Options struct {
	Store     Store
	Snapshots Snapshots
	// Runner runs the light model (the metered runner: every run is
	// recorded for /usage under the attribution of Attribute).
	Runner core.Runner
	Model  string
	// ScratchDir is where tool-less runs get their empty working directory.
	ScratchDir string
	// Interval between passes (default 10m).
	Interval time.Duration
	// InitialDelay before the first pass of Run (default 1m; negative = none).
	InitialDelay time.Duration
	// MaxRunsPerHour caps extraction runs (sliding hour, counted from the
	// stored run records, so restarts do not reset it). 0 = code is never
	// sent to the model: only OpenAPI specs are mapped.
	MaxRunsPerHour int
	// DepsMaxRunsPerHour caps phase B runs (files of merge requests) with
	// a budget of their own, so building the map and reviews do not take
	// runs from each other. 0 = phase B never sends code to the model; it
	// is off as well when MaxRunsPerHour is 0.
	DepsMaxRunsPerHour int
	// MaxFilesPerRepo caps the code candidates of one repository (default
	// 400); MaxFileBytes skips larger files (default 64 KB).
	MaxFilesPerRepo int
	MaxFileBytes    int64
	// MaxBatchChars / MaxBatchFiles bound one run's prompt.
	MaxBatchChars int
	MaxBatchFiles int
	RunTimeout    time.Duration
	// Exclude drops repositories (service-level globs); excluded ones are
	// removed from the map.
	Exclude func(repo string) bool
	// Hints map a base expression or env var name to a repository path.
	Hints map[string]string
	// Topology: review.sysmap.topology, the last matching step (index.go).
	Topology []TopologyRule
	// Externals: review.sysmap.externals, applied before hints (index.go).
	Externals []ExternalRule
	// GitLabURL (gitlab.url): calls to hosts under its domain
	// (OwnDomain) are never external.
	GitLabURL string
	// Attribute returns the context a run of repo is charged under
	// (meter.With(... KindMap ...)); nil = ctx unchanged.
	Attribute func(ctx context.Context, repo string) context.Context
	// OnScan, when set, is called after every repository update with the
	// resulting status (metrics).
	OnScan func(repo, status string)
	Logger *slog.Logger

	Now func() time.Time
}

// Map keeps the system map up to date and serves the last built Index.
type Map struct {
	o      Options
	log    *slog.Logger
	idx    atomic.Pointer[Index]
	passMu sync.Mutex
	// missing: since when a stored repository has not been among the
	// snapshots (guarded by passMu).
	missing map[string]time.Time
}

// New returns a Map. Call Load (or Run) to serve the stored map.
func New(o Options) *Map {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.InitialDelay == 0 {
		o.InitialDelay = defaultInitialDelay
	}
	if o.MaxFilesPerRepo <= 0 {
		o.MaxFilesPerRepo = DefaultMaxFilesPerRepo
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	if o.MaxBatchChars <= 0 {
		o.MaxBatchChars = defaultBatchChars
	}
	// A file at the size cap (plus line prefixes) must fit into one batch.
	if minChars := int(o.MaxFileBytes) * 6 / 5; o.MaxBatchChars < minChars {
		o.MaxBatchChars = minChars
	}
	if o.MaxBatchFiles <= 0 {
		o.MaxBatchFiles = defaultBatchFiles
	}
	if o.RunTimeout <= 0 {
		o.RunTimeout = defaultRunTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Map{o: o, log: o.Logger, missing: map[string]time.Time{}}
}

// Index returns the last built index (nil before the first Load).
func (m *Map) Index() *Index {
	if m == nil {
		return nil
	}
	return m.idx.Load()
}

// Impact is Index().Impact (nil when nothing is loaded).
func (m *Map) Impact(repo string, changed map[string][]LineRange, endpoints []EndpointRef) *Impact {
	return m.Index().Impact(repo, changed, endpoints)
}

// RunStats returns the extraction runs since t (for the /map page).
func (m *Map) RunStats(ctx context.Context, since time.Time) (RunStats, error) {
	return m.o.Store.SysmapRunStats(ctx, since)
}

// MaxRunsPerHour is the configured budget.
func (m *Map) MaxRunsPerHour() int { return m.o.MaxRunsPerHour }

// Load rebuilds the index from the store.
func (m *Map) Load(ctx context.Context) error {
	repos, err := m.o.Store.ListSysmapRepos(ctx)
	if err != nil {
		return fmt.Errorf("sysmap: list repos: %w", err)
	}
	files, err := m.o.Store.ListAllSysmapFiles(ctx)
	if err != nil {
		return fmt.Errorf("sysmap: list files: %w", err)
	}
	x := build(repos, files, m.o.Hints, OwnDomain(m.o.GitLabURL), m.o.Topology, m.o.Externals, m.o.Now())
	m.idx.Store(x)
	return nil
}

// Run loads the stored map and then updates it every Interval until ctx is
// done. Errors are logged; the last good index keeps being served.
func (m *Map) Run(ctx context.Context) {
	if err := m.Load(ctx); err != nil {
		m.log.Warn("sysmap: load", "err", err)
	}
	if m.o.InitialDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.o.InitialDelay):
		}
	}
	t := time.NewTicker(m.o.Interval)
	defer t.Stop()
	for {
		if err := m.Pass(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("sysmap: pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// HintsList returns the configured hints sorted by key (for the page).
func (m *Map) HintsList() [][2]string {
	var out [][2]string
	for k, v := range m.o.Hints {
		out = append(out, [2]string{k, strings.Trim(v, "/")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
