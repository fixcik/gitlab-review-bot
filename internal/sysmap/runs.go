package sysmap

// Model runs: the hourly budgets per run kind and one extraction run of the
// light model (recorded in sysmap_runs whatever its outcome).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// runLimit is the hourly budget of a run kind (0 = no code to the model).
func (m *Map) runLimit(kind string) int {
	if m.o.MaxRunsPerHour <= 0 {
		return 0
	}
	if kind == RunKindDeps {
		return max(0, m.o.DepsMaxRunsPerHour)
	}
	return m.o.MaxRunsPerHour
}

// runsLeft is the kind's hourly budget minus its runs of the last hour.
func (m *Map) runsLeft(ctx context.Context, kind string) (int, error) {
	limit := m.runLimit(kind)
	if limit <= 0 {
		return 0, nil
	}
	n, err := m.o.Store.SysmapRunCount(ctx, m.o.Now().Add(-time.Hour), kind)
	if err != nil {
		return 0, fmt.Errorf("run stats: %w", err)
	}
	return max(0, limit-n), nil
}

// extract runs the light model on one batch and returns the file records.
// A run error is returned (the files stay pending); an unusable answer
// gives records with Error set (not retried until the content changes).
func (m *Map) extract(ctx context.Context, repo string, batch []fileContent) ([]FileRecord, error) {
	return m.extractWith(ctx, repo, whereDefault, nil, batch)
}

// extractWith is extract with the source named in the prompt (where) and
// an attribution override (nil = Options.Attribute).
func (m *Map) extractWith(ctx context.Context, repo, where string, attribute func(context.Context) context.Context,
	batch []fileContent) ([]FileRecord, error) {
	dir, err := m.scratchDir()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	rctx := ctx
	switch {
	case attribute != nil:
		rctx = attribute(ctx)
	case m.o.Attribute != nil:
		rctx = m.o.Attribute(ctx, repo)
	}
	start := m.o.Now()
	res, err := m.o.Runner.Run(rctx, core.RunRequest{
		Dir: dir, Prompt: extractPrompt(repo, where, batch), SystemPrompt: extractSystemPrompt,
		JSONSchema: ExtractSchema, Model: m.o.Model, Timeout: m.o.RunTimeout,
		NoTools: true,
	})
	rec := RunRecord{At: start, Repo: repo, Files: len(batch), OK: err == nil, Duration: m.o.Now().Sub(start), Kind: RunKindScan}
	if where == whereMR {
		rec.Kind = RunKindDeps
	}
	metrics := res
	if metrics == nil {
		var fr *core.FailedRun
		if errors.As(err, &fr) {
			metrics = &fr.Partial
		}
	}
	if metrics != nil {
		rec.CostUSD, rec.Tokens = metrics.CostUSD, metrics.Usage.Total()
	}
	if err != nil {
		rec.Error = clip(err.Error(), 300)
	}
	var parsed map[string][]Entry
	var perr error
	if err == nil {
		parsed, perr = parseExtraction(res.Output, batch)
		if perr != nil {
			rec.OK, rec.Error = false, clip(perr.Error(), 300)
		}
	}
	// Record even when the job's context was cancelled: the tokens are spent.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if serr := m.o.Store.RecordSysmapRun(sctx, rec); serr != nil {
		m.log.Warn("sysmap: record run", "err", serr)
	}
	if err != nil {
		return nil, err
	}
	now := m.o.Now()
	out := make([]FileRecord, 0, len(batch))
	for _, f := range batch {
		r := FileRecord{Path: f.Path, Hash: f.Hash, Version: CodeVersion, ExtractedAt: now, Entries: []Entry{}}
		if perr != nil {
			r.Error = clip(perr.Error(), 300)
		} else {
			r.Entries = parsed[f.Path]
		}
		out = append(out, r)
	}
	if perr != nil {
		m.log.Warn("sysmap: unusable extraction answer", "repo", repo, "files", len(batch), "err", perr)
	}
	return out, nil
}

func (m *Map) scratchDir() (string, error) {
	base := m.o.ScratchDir
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("scratch dir: %w", err)
	}
	return os.MkdirTemp(base, "sysmap-")
}
