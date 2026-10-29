package main

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/mapweb"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// startSysmap starts the system map (review.sysmap): a background scanner
// over the context repository snapshots. It returns nil when the feature
// is off (it needs context repositories).
func startSysmap(ctx context.Context, cfg *config.Service, snaps crossrepo.ContextRepos, st *store.SQLite, runner core.Runner,
	dataDir string, tm *telemetry.Metrics, probs *problems.Service, logger *slog.Logger) *sysmap.Map {
	c := cfg.Review.Sysmap
	if !cfg.SysmapEnabled() || snaps == nil {
		if c.Enabled {
			logger.Info("system map off: it needs context repositories (review.context and gitlab.groups)")
		}
		return nil
	}
	var exclude func(string) bool
	if len(c.Exclude) > 0 {
		exclude = c.Excluded
	}
	m := sysmap.New(sysmap.Options{
		Store: st, Snapshots: snaps,
		// The metered runner: every extraction run shows up on /usage as
		// "map" (not counted against limits.global_runs_per_hour).
		Runner: runner, Model: cfg.SysmapModel(),
		ScratchDir:     filepath.Join(dataDir, "scratch"),
		Interval:       c.RefreshInterval.D(),
		MaxRunsPerHour: c.MaxRunsPerHour,
		// Phase B has its own budget: reviews and the map do not compete.
		DepsMaxRunsPerHour: c.DepsMaxRunsPerHour,
		MaxFilesPerRepo:    c.MaxFilesPerRepo,
		MaxFileBytes:       c.MaxFileBytes(),
		Exclude:            exclude,
		Hints:              c.Hints,
		Topology:           sysmap.TopologyFromConfig(c.Topology),
		Externals:          sysmap.ExternalsFromConfig(c.Externals),
		GitLabURL:          cfg.GitLab.URL,
		Attribute: func(ctx context.Context, repo string) context.Context {
			return meter.With(ctx, meter.Attribution{Kind: meter.KindMap, ProjectPath: repo})
		},
		OnScan: sysmapProblems(probs, tm.ObserveSysmapScan), // metric + scan errors as problems
		Logger: logger,
	})
	tm.ObserveSysmap(m)
	logger.Info("system map enabled", "model", cfg.SysmapModel(), "max_runs_per_hour", c.MaxRunsPerHour,
		"deps_max_runs_per_hour", c.DepsMaxRunsPerHour,
		"max_files_per_repo", c.MaxFilesPerRepo, "max_file_kb", c.MaxFileKB, "hints", len(c.Hints), "deps", c.Deps)
	go m.Run(ctx)
	return m
}

// The map also answers phase B (the consumer side of a review).
var _ crossrepo.SystemMapDeps = (*sysmap.Map)(nil)

// systemMapDep is the reviewer's view of the map (a nil interface when off).
func systemMapDep(m *sysmap.Map) crossrepo.SystemMap {
	if m == nil {
		return nil
	}
	return m
}

// registerMapWeb mounts /map (404 when the map is off).
func registerMapWeb(mux *http.ServeMux, cfg *config.Service, m *sysmap.Map, nav webui.Nav, logger *slog.Logger) {
	o := mapweb.Options{Nav: nav, Enabled: m != nil, Token: cfg.Review.Sysmap.AccessToken(cfg.Usage.Token), GitLabURL: cfg.GitLab.URL,
		MaxHintKey: config.MaxSysmapHintKeyLen, Logger: logger}
	if m != nil {
		o.Map = m
	}
	mapweb.Register(mux, o)
}
