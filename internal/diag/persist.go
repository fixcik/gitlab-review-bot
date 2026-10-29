package diag

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// File names inside a run directory.
const (
	RecordFile = "run.json"
	// transcriptFmt is claude-<n>.jsonl, n starting at 1.
	transcriptFmt = "claude-%d.jsonl"
)

// TranscriptName is the file name of the n-th Claude transcript.
func TranscriptName(n int) string { return fmt.Sprintf(transcriptFmt, n) }

// minJSONBudget is what run.json may always use, whatever the transcripts
// took.
const minJSONBudget = 256 << 10

// flushLocked writes run.json and the index row (throttled unless force).
// Called with run.mu held.
func (run *Run) flushLocked(force bool) {
	now := run.rec.o.Now()
	if !force && now.Sub(run.lastWrite) < writeEvery {
		return
	}
	run.lastWrite = now
	b, truncated := run.encodeLocked()
	if truncated {
		run.r.Truncated = true
	}
	if !run.writeError {
		if err := writeFileAtomic(filepath.Join(run.dir, RecordFile), b); err != nil {
			run.rec.log.Warn("diag: write run.json", "run", run.r.ID, "err", err)
		}
	}
	row := run.indexRowLocked(int64(len(b)) + run.tBytes)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := run.rec.o.Index.SaveDiagRun(ctx, row); err != nil {
		run.rec.log.Warn("diag: save index", "run", run.r.ID, "err", err)
	}
}

// encodeLocked marshals the record, shrinking long strings until it fits
// the run's budget (diag.max_run_mb minus the transcripts).
func (run *Run) encodeLocked() ([]byte, bool) {
	budget := run.rec.o.MaxRunBytes - run.tBytes
	if budget < minJSONBudget {
		budget = minJSONBudget
	}
	b, _ := json.MarshalIndent(&run.r, "", " ")
	if int64(len(b)) <= budget {
		return b, false
	}
	// Shrink a copy: the in-memory record keeps everything so later
	// flushes (with more budget) are not lossy twice.
	for limit := 1 << 20; limit >= 512; limit /= 2 {
		cp := shrinkRecord(run.r, limit)
		cp.Truncated = true
		b, _ = json.MarshalIndent(&cp, "", " ")
		if int64(len(b)) <= budget {
			return b, true
		}
	}
	return b, true
}

// truncMarker is inserted where a stored string was cut.
const truncMarker = "\n…[обрезано: превышен diag.max_run_mb]…\n"

func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	h := limit * 2 / 3
	t := limit - h
	return s[:h] + truncMarker + s[len(s)-t:]
}

func shrinkRecord(r Record, limit int) Record {
	r.Steps = append([]TimelineStep(nil), r.Steps...)
	for i := range r.Steps {
		s := &r.Steps[i]
		s.Stdout, s.Stderr, s.Detail, s.Error = cut(s.Stdout, limit), cut(s.Stderr, limit), cut(s.Detail, limit), cut(s.Error, limit)
	}
	r.Claude = append([]ClaudeRun(nil), r.Claude...)
	for i := range r.Claude {
		c := &r.Claude[i]
		c.Prompt, c.SystemPrompt, c.JSONSchema = cut(c.Prompt, limit), cut(c.SystemPrompt, limit), cut(c.JSONSchema, limit)
		c.Output, c.Stderr, c.Error = cut(c.Output, limit), cut(c.Stderr, limit), cut(c.Error, limit)
	}
	r.Job.Question = cut(r.Job.Question, limit)
	return r
}

func (run *Run) indexRowLocked(size int64) IndexRow {
	r := &run.r
	row := IndexRow{
		ID: r.ID, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Kind: r.Job.Kind,
		ProjectID: r.Job.ProjectID, ProjectPath: r.Job.ProjectPath, MRIID: r.Job.MRIID, MRTitle: r.Job.MRTitle,
		MRURL: r.Job.MRURL, Username: r.Job.Username, Status: r.Status, Reason: r.Reason, Error: firstLine(r.Error, 500),
		RetryAt: r.RetryAt, Attempt: r.Attempt, DurationMS: r.DurationMS, ClaudeRuns: len(r.Claude),
		SizeBytes: size, Dir: filepath.ToSlash(run.relDir),
	}
	if row.Username == "" {
		row.Username = r.Job.MRAuthor
	}
	if row.FinishedAt.IsZero() {
		row.DurationMS = run.rec.o.Now().Sub(r.StartedAt).Milliseconds()
	}
	for _, c := range r.Claude {
		row.Tokens += c.Usage.Total()
		row.CostUSD += c.CostUSD
	}
	return row
}

func firstLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// Retention and size limits
// ---------------------------------------------------------------------------

// CleanupInterval is how often Run applies retention.
const CleanupInterval = 10 * time.Minute

// Run applies retention at start and every CleanupInterval until ctx is
// done.
func (rec *Recorder) Run(ctx context.Context) {
	if rec == nil {
		return
	}
	t := time.NewTicker(CleanupInterval)
	defer t.Stop()
	for {
		rec.Cleanup(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// markInterrupted turns rows left "running" by a previous process into
// "interrupted" (called from New, before any job of this process starts).
func (rec *Recorder) markInterrupted() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := rec.o.Index.MarkDiagRunsInterrupted(ctx)
	if err != nil {
		rec.log.Warn("diag: mark interrupted runs", "err", err)
	} else if n > 0 {
		rec.log.Info("diag: runs interrupted by restart", "count", n)
	}
}

// Cleanup removes runs older than the retention, orphan directories, and
// the oldest runs while the total size exceeds diag.max_total_mb.
func (rec *Recorder) Cleanup(ctx context.Context) {
	if rec == nil {
		return
	}
	cutoff := rec.o.Now().Add(-rec.o.Retention)
	old, err := rec.o.Index.ListDiagRuns(ctx, IndexFilter{Before: cutoff, OldestFirst: true})
	if err != nil {
		rec.log.Warn("diag: list expired runs", "err", err)
	}
	removed := 0
	for _, r := range old {
		if rec.isActive(r.ID) {
			continue
		}
		rec.remove(ctx, r)
		removed++
	}
	removed += rec.removeOrphans(ctx, cutoff)
	if removed > 0 {
		rec.log.Info("diag: expired runs removed", "count", removed)
	}
	rec.enforceTotal(ctx, "")
}

func (rec *Recorder) isActive(id string) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, r := range rec.active {
		if r.r.ID == id {
			return true
		}
	}
	return false
}

func (rec *Recorder) remove(ctx context.Context, r IndexRow) {
	if dir := rec.safeDir(r.Dir); dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			rec.log.Warn("diag: remove run dir", "run", r.ID, "err", err)
			return
		}
		_ = os.Remove(filepath.Dir(dir)) // the day dir, if now empty
	}
	if err := rec.o.Index.DeleteDiagRun(ctx, r.ID); err != nil {
		rec.log.Warn("diag: delete index row", "run", r.ID, "err", err)
	}
}

// safeDir resolves an index Dir under the root ("" if it would escape).
func (rec *Recorder) safeDir(rel string) string {
	return safeJoin(rec.o.Root, rel)
}

func safeJoin(root, rel string) string {
	if rel == "" {
		return ""
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	r, err := filepath.Rel(root, p)
	if err != nil || r == "." || strings.HasPrefix(r, "..") {
		return ""
	}
	return p
}

// removeOrphans deletes run directories older than cutoff that have no index
// row (e.g. the index was reset) and empty day directories.
func (rec *Recorder) removeOrphans(ctx context.Context, cutoff time.Time) int {
	days, err := os.ReadDir(rec.o.Root)
	if err != nil {
		return 0
	}
	n := 0
	for _, d := range days {
		if !d.IsDir() || len(d.Name()) != 8 {
			continue
		}
		dayDir := filepath.Join(rec.o.Root, d.Name())
		runs, _ := os.ReadDir(dayDir)
		for _, r := range runs {
			if !r.IsDir() {
				continue
			}
			info, err := r.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			row, err := rec.o.Index.GetDiagRun(ctx, r.Name())
			if err != nil || row != nil {
				continue
			}
			if os.RemoveAll(filepath.Join(dayDir, r.Name())) == nil {
				n++
			}
		}
		_ = os.Remove(dayDir) // only succeeds when empty
	}
	return n
}

// enforceTotal evicts the oldest finished runs while the total exceeds
// diag.max_total_mb; keep is never evicted (the run that just finished).
func (rec *Recorder) enforceTotal(ctx context.Context, keep string) {
	if rec == nil {
		return
	}
	total, err := rec.o.Index.DiagTotalSize(ctx)
	if err != nil || total <= rec.o.MaxTotalBytes {
		return
	}
	rows, err := rec.o.Index.ListDiagRuns(ctx, IndexFilter{OldestFirst: true})
	if err != nil {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartedAt.Before(rows[j].StartedAt) })
	evicted := 0
	for _, r := range rows {
		if total <= rec.o.MaxTotalBytes {
			break
		}
		if r.ID == keep || rec.isActive(r.ID) {
			continue
		}
		rec.remove(ctx, r)
		total -= r.SizeBytes
		evicted++
	}
	if evicted > 0 {
		rec.log.Info("diag: oldest runs evicted (diag.max_total_mb)", "count", evicted)
	}
}
