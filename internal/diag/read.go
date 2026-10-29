package diag

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Reader reads stored runs (used by the /runs pages and `reviewbot runs`).
type Reader struct {
	Root  string
	Index Index
}

// Reader returns a reader over this recorder's data.
func (rec *Recorder) Reader() *Reader {
	if rec == nil {
		return nil
	}
	return &Reader{Root: rec.o.Root, Index: rec.o.Index}
}

var idRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{8}$`)

// ValidID reports whether id has the run id format.
func ValidID(id string) bool { return idRe.MatchString(id) }

// List returns index rows.
func (r *Reader) List(ctx context.Context, f IndexFilter) ([]IndexRow, error) {
	return r.Index.ListDiagRuns(ctx, f)
}

// Get returns the index row and run.json of id; nil, nil, nil when unknown.
func (r *Reader) Get(ctx context.Context, id string) (*IndexRow, *Record, error) {
	if !ValidID(id) {
		return nil, nil, nil
	}
	row, err := r.Index.GetDiagRun(ctx, id)
	if err != nil || row == nil {
		return row, nil, err
	}
	dir := safeJoin(r.Root, row.Dir)
	if dir == "" {
		return row, nil, fmt.Errorf("diag: bad run dir %q", row.Dir)
	}
	b, err := os.ReadFile(filepath.Join(dir, RecordFile))
	if err != nil {
		return row, nil, fmt.Errorf("diag: read run.json: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return row, nil, fmt.Errorf("diag: parse run.json: %w", err)
	}
	return row, &rec, nil
}

// TranscriptPath is the path of claude-<n>.jsonl of a run ("" if invalid).
func (r *Reader) TranscriptPath(row *IndexRow, n int) string {
	if row == nil || n < 1 || n > 1000 {
		return ""
	}
	dir := safeJoin(r.Root, row.Dir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, TranscriptName(n))
}
