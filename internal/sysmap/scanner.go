package sysmap

// The scan pass: which repositories changed since their last complete pass,
// what of them to (re)extract within the budget, and saving the results.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// version identifies the extractors and the file selection settings
// (max_files_per_repo, max_file_kb) of a complete pass: a change of either
// re-plans the repository at the same SHA. Re-planning is cheap: only files
// without a stored record of the same hash and extractor version go to the
// model, so a state stored before the settings were part of the version
// (one re-plan after the upgrade) costs no runs unless the selection grew.
func (m *Map) version() string {
	sel := fmt.Sprintf("+sel%d/%d", m.o.MaxFilesPerRepo, m.o.MaxFileBytes)
	if m.o.MaxRunsPerHour <= 0 {
		return SpecVersion + "+nocode" + sel
	}
	return SpecVersion + "+" + CodeVersion + sel
}

// repoPlan is the work for one repository in a pass.
type repoPlan struct {
	dir     core.ContextDir
	state   RepoState
	sel     selection
	specs   []FileRecord  // parsed specs to save
	todo    []fileContent // code files to extract (content in memory)
	removed []string
	pending int // files needing extraction that were not loaded
}

// Pass updates every repository whose default-branch SHA (or extractor
// version) changed since its last complete pass. Snapshots are held only
// while files are hashed and the changed ones read; the model runs after
// they are released. Extraction runs stop at MaxRunsPerHour; the rest
// waits for a later pass (status "partial").
func (m *Map) Pass(ctx context.Context) error {
	m.passMu.Lock()
	defer m.passMu.Unlock()
	start := m.o.Now()

	byPath, err := m.storedRepos(ctx)
	if err != nil {
		return err
	}
	runsLeft, err := m.runsLeft(ctx, RunKindScan)
	if err != nil {
		return err
	}
	// Characters of code that this pass may read into memory: what the
	// remaining runs can extract.
	charBudget := runsLeft * m.o.MaxBatchChars

	dirs, _, release := m.o.Snapshots.Acquire(ctx, core.ContextRequest{Exclude: m.o.Exclude})
	if release == nil {
		release = func() {}
	}
	m.dropStale(ctx, byPath, dirs)
	// Oldest scan first, never scanned before everything else.
	sort.SliceStable(dirs, func(i, j int) bool {
		a, b := byPath[lower(dirs[i].Name)].ScannedAt, byPath[lower(dirs[j].Name)].ScannedAt
		if !a.Equal(b) {
			return a.Before(b)
		}
		return lower(dirs[i].Name) < lower(dirs[j].Name)
	})
	plans := m.planChanged(ctx, dirs, byPath, &charBudget)
	release()

	stopped := false // rate limited: no more runs this pass
	runs := 0
	for _, p := range plans {
		if ctx.Err() != nil {
			break
		}
		n, stop := m.execute(ctx, p, &runsLeft, stopped)
		runs += n
		stopped = stopped || stop
	}
	if err := m.Load(ctx); err != nil {
		return err
	}
	if len(plans) > 0 {
		m.log.Info("sysmap: pass done", "repos", len(dirs), "updated", len(plans), "runs", runs,
			"took", m.o.Now().Sub(start).Round(time.Millisecond))
	}
	return ctx.Err()
}

// storedRepos returns the stored repository states by lower-case path,
// deleting the ones the service excludes.
func (m *Map) storedRepos(ctx context.Context) (map[string]RepoState, error) {
	states, err := m.o.Store.ListSysmapRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	byPath := map[string]RepoState{}
	for _, s := range states {
		if m.o.Exclude != nil && m.o.Exclude(s.Path) {
			if err := m.o.Store.DeleteSysmapRepo(ctx, s.Path); err != nil {
				m.log.Warn("sysmap: delete excluded repo", "repo", s.Path, "err", err)
			}
			continue
		}
		byPath[lower(s.Path)] = s
	}
	return byPath, nil
}

// planChanged plans every snapshot whose SHA or extractor version changed
// since its last complete pass (a repository that cannot be planned is
// saved with StatusError).
func (m *Map) planChanged(ctx context.Context, dirs []core.ContextDir, byPath map[string]RepoState, charBudget *int) []*repoPlan {
	var plans []*repoPlan
	ver := m.version()
	for _, d := range dirs {
		if ctx.Err() != nil {
			break
		}
		st, known := byPath[lower(d.Name)]
		if known && st.SHA == d.SHA && st.Status == StatusOK && st.Version == ver {
			continue
		}
		p, err := m.plan(ctx, d, st, charBudget)
		if err != nil {
			m.log.Warn("sysmap: plan repo", "repo", d.Name, "err", err)
			st.Path, st.SHA, st.Branch, st.ScannedAt = d.Name, d.SHA, d.Branch, m.o.Now()
			st.Status, st.Error = StatusError, clip(err.Error(), 300)
			if serr := m.o.Store.SaveSysmapRepo(ctx, st); serr != nil {
				m.log.Warn("sysmap: save repo state", "repo", d.Name, "err", serr)
			}
			m.onScan(d.Name, StatusError)
			continue
		}
		plans = append(plans, p)
	}
	return plans
}

// dropStale removes repositories that have been missing from the
// snapshots for staleAfter. A pass without any snapshot (provider trouble)
// does not count.
func (m *Map) dropStale(ctx context.Context, states map[string]RepoState, dirs []core.ContextDir) {
	if len(dirs) == 0 {
		return
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		seen[lower(d.Name)] = true
		delete(m.missing, lower(d.Name))
	}
	now := m.o.Now()
	for k, st := range states {
		if seen[k] {
			continue
		}
		since, ok := m.missing[k]
		if !ok {
			m.missing[k] = now
			continue
		}
		if now.Sub(since) < staleAfter {
			continue
		}
		if err := m.o.Store.DeleteSysmapRepo(ctx, st.Path); err != nil {
			m.log.Warn("sysmap: delete stale repo", "repo", st.Path, "err", err)
			continue
		}
		delete(m.missing, k)
		delete(states, k)
		m.log.Info("sysmap: repository gone from the groups, dropped from the map", "repo", st.Path, "missing_since", since)
	}
}

func (m *Map) onScan(repo, status string) {
	if m.o.OnScan != nil {
		m.o.OnScan(repo, status)
	}
}

// plan walks one snapshot: specs are parsed right away, changed code files
// are read while charBudget lasts.
func (m *Map) plan(ctx context.Context, d core.ContextDir, st RepoState, charBudget *int) (*repoPlan, error) {
	sel, err := selectFiles(d.HostPath, m.o.MaxFilesPerRepo, m.o.MaxFileBytes, nil)
	if err != nil {
		return nil, fmt.Errorf("walk snapshot: %w", err)
	}
	stored, err := m.o.Store.ListSysmapFiles(ctx, d.Name)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	have := make(map[string]FileRecord, len(stored))
	for _, f := range stored {
		have[f.Path] = f
	}
	p := &repoPlan{dir: d, state: st, sel: sel}
	keep := map[string]bool{}
	for _, c := range sel.Specs {
		keep[c.Path] = true
		if f, ok := have[c.Path]; ok && f.Hash == c.Hash && f.Version == SpecVersion {
			continue
		}
		rec := FileRecord{Path: c.Path, Hash: c.Hash, Version: SpecVersion, ExtractedAt: m.o.Now(), Entries: []Entry{}}
		b, err := readSnapshotFile(d.HostPath, c.Path, maxSpecBytes)
		if err == nil {
			rec.Entries, err = ParseSpec(c.Path, b)
		}
		if err != nil {
			rec.Error = clip(err.Error(), 300)
		}
		p.specs = append(p.specs, rec)
	}
	if m.o.MaxRunsPerHour > 0 {
		for _, c := range sel.Code {
			keep[c.Path] = true
			if f, ok := have[c.Path]; ok && f.Hash == c.Hash && f.Version == CodeVersion {
				continue
			}
			if *charBudget <= 0 {
				p.pending++
				continue
			}
			b, err := readSnapshotFile(d.HostPath, c.Path, m.o.MaxFileBytes)
			if err != nil {
				m.log.Warn("sysmap: read file", "repo", d.Name, "file", c.Path, "err", err)
				continue
			}
			if hashOf(b) != c.Hash { // changed under us; next pass
				p.pending++
				continue
			}
			text, n := numbered(string(b))
			p.todo = append(p.todo, fileContent{candidate: c, text: text, lines: n})
			*charBudget -= len(text)
		}
	}
	for path := range have {
		if !keep[path] {
			p.removed = append(p.removed, path)
		}
	}
	sort.Strings(p.removed)
	return p, nil
}

// readSnapshotFile reads a regular file of a snapshot (no symlinks).
func readSnapshotFile(root, rel string, maxBytes int64) ([]byte, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if maxBytes > 0 && fi.Size() > maxBytes {
		return nil, errors.New("file too large")
	}
	return os.ReadFile(p)
}

// execute saves a plan's specs, removes deleted files, runs the extraction
// batches within the budget and saves the repository state. It returns the
// number of runs and whether the model is rate limited.
func (m *Map) execute(ctx context.Context, p *repoPlan, runsLeft *int, stopped bool) (int, bool) {
	repo := p.dir.Name
	log := m.log.With("repo", repo)
	if len(p.specs) > 0 {
		if err := m.o.Store.SaveSysmapFiles(ctx, repo, p.specs); err != nil {
			log.Warn("sysmap: save specs", "err", err)
		}
	}
	if len(p.removed) > 0 {
		if err := m.o.Store.DeleteSysmapFiles(ctx, repo, p.removed); err != nil {
			log.Warn("sysmap: delete removed files", "err", err)
		}
	}
	pending := p.pending
	runs := 0
	var lastErr string
	for _, b := range batches(p.todo, m.o.MaxBatchChars, m.o.MaxBatchFiles) {
		if stopped || *runsLeft <= 0 || ctx.Err() != nil {
			pending += len(b)
			continue
		}
		*runsLeft--
		runs++
		recs, err := m.extract(ctx, repo, b)
		var rl *core.ErrClaudeRateLimited
		switch {
		case errors.As(err, &rl):
			log.Warn("sysmap: claude rate limited, stopping this pass", "err", err)
			stopped = true
			pending += len(b)
			lastErr = "лимит Claude"
			continue
		case err != nil:
			log.Warn("sysmap: extraction run failed", "files", len(b), "err", err)
			pending += len(b)
			lastErr = clip(err.Error(), 300)
			continue
		}
		if err := m.o.Store.SaveSysmapFiles(ctx, repo, recs); err != nil {
			log.Warn("sysmap: save files", "err", err)
			pending += len(b)
		}
	}
	st := p.state
	st.Path, st.SHA, st.Branch, st.ScannedAt = repo, p.dir.SHA, p.dir.Branch, m.o.Now()
	st.Candidates, st.Specs, st.Capped, st.TooLarge, st.Pending = len(p.sel.Code), len(p.sel.Specs), p.sel.Capped, p.sel.TooLarge, pending
	if m.o.MaxRunsPerHour <= 0 {
		st.Candidates = 0
	}
	st.Error = lastErr
	if pending == 0 {
		st.Status, st.Version = StatusOK, m.version()
	} else {
		st.Status = StatusPartial
	}
	if err := m.o.Store.SaveSysmapRepo(ctx, st); err != nil {
		log.Warn("sysmap: save repo state", "err", err)
	}
	m.onScan(repo, st.Status)
	return runs, stopped
}
