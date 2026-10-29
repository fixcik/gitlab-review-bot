package store

// Index of diagnostic run records (internal/diag). The records themselves
// live on disk under <data_dir>/diag; this table only lists them for the
// /runs page, the `reviewbot runs` command and the retention cleanup.
//
// The table comes from a named migration registered from this file (init
// appends to namedMigrations), so neither the sequential schema_version list
// nor the other features' migration list has to be edited.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/diag/diagindex"
)

func init() {
	addNamedMigration("diag_runs_v1", `
	CREATE TABLE IF NOT EXISTS diag_runs (
		id           TEXT    PRIMARY KEY,
		started_at   INTEGER NOT NULL,
		finished_at  INTEGER NOT NULL DEFAULT 0,
		kind         TEXT    NOT NULL DEFAULT '',
		project_id   INTEGER NOT NULL DEFAULT 0,
		project_path TEXT    NOT NULL DEFAULT '',
		mr_iid       INTEGER NOT NULL DEFAULT 0,
		mr_title     TEXT    NOT NULL DEFAULT '',
		mr_url       TEXT    NOT NULL DEFAULT '',
		username     TEXT    NOT NULL DEFAULT '',
		status       TEXT    NOT NULL DEFAULT '',
		reason       TEXT    NOT NULL DEFAULT '',
		error        TEXT    NOT NULL DEFAULT '',
		retry_at     INTEGER NOT NULL DEFAULT 0,
		attempt      INTEGER NOT NULL DEFAULT 1,
		duration_ms  INTEGER NOT NULL DEFAULT 0,
		claude_runs  INTEGER NOT NULL DEFAULT 0,
		tokens       INTEGER NOT NULL DEFAULT 0,
		cost_usd     REAL    NOT NULL DEFAULT 0,
		size_bytes   INTEGER NOT NULL DEFAULT 0,
		dir          TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS diag_runs_started ON diag_runs(started_at);
	CREATE INDEX IF NOT EXISTS diag_runs_mr ON diag_runs(project_path, mr_iid);`)
}

const diagCols = `id, started_at, finished_at, kind, project_id, project_path, mr_iid, mr_title, mr_url, username,
	status, reason, error, retry_at, attempt, duration_ms, claude_runs, tokens, cost_usd, size_bytes, dir`

// SaveDiagRun inserts or replaces a row.
func (s *SQLite) SaveDiagRun(ctx context.Context, r diagindex.Row) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO diag_runs(`+diagCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, toNanos(r.StartedAt), toNanos(r.FinishedAt), r.Kind, r.ProjectID, r.ProjectPath, r.MRIID, r.MRTitle, r.MRURL,
		r.Username, r.Status, r.Reason, r.Error, toNanos(r.RetryAt), r.Attempt, r.DurationMS, r.ClaudeRuns, r.Tokens,
		r.CostUSD, r.SizeBytes, r.Dir)
	if err != nil {
		return fmt.Errorf("store: save diag run: %w", err)
	}
	return nil
}

func scanDiagRun(sc rowScanner) (diagindex.Row, error) {
	var (
		r                        diagindex.Row
		started, finished, retry int64
	)
	if err := sc.Scan(&r.ID, &started, &finished, &r.Kind, &r.ProjectID, &r.ProjectPath, &r.MRIID, &r.MRTitle, &r.MRURL,
		&r.Username, &r.Status, &r.Reason, &r.Error, &retry, &r.Attempt, &r.DurationMS, &r.ClaudeRuns, &r.Tokens,
		&r.CostUSD, &r.SizeBytes, &r.Dir); err != nil {
		return diagindex.Row{}, err
	}
	r.StartedAt, r.FinishedAt, r.RetryAt = fromNanos(started), fromNanos(finished), fromNanos(retry)
	return r, nil
}

// GetDiagRun returns one row; nil, nil when there is none.
func (s *SQLite) GetDiagRun(ctx context.Context, id string) (*diagindex.Row, error) {
	r, err := scanDiagRun(s.db.QueryRowContext(ctx, `SELECT `+diagCols+` FROM diag_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get diag run: %w", err)
	}
	return &r, nil
}

// ListDiagRuns returns matching rows, newest first (or oldest first).
func (s *SQLite) ListDiagRuns(ctx context.Context, f diagindex.Filter) ([]diagindex.Row, error) {
	var w whereClause
	if !f.Since.IsZero() {
		w.add("started_at >= ?", toNanos(f.Since))
	}
	if !f.Before.IsZero() {
		w.add("started_at < ?", toNanos(f.Before))
	}
	if f.Status != "" {
		w.add("status = ?", f.Status)
	}
	if f.Kind != "" {
		w.add("kind = ?", f.Kind)
	}
	if p := strings.TrimSpace(f.ProjectPath); p != "" {
		w.add(`LOWER(project_path) LIKE ? ESCAPE '\'`, "%"+likeEscape(strings.ToLower(p))+"%")
	}
	if f.MRIID > 0 {
		w.add("mr_iid = ?", f.MRIID)
	}
	q := `SELECT ` + diagCols + ` FROM diag_runs` + w.sql()
	if f.OldestFirst {
		q += " ORDER BY started_at ASC, id ASC"
	} else {
		q += " ORDER BY started_at DESC, id DESC"
	}
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, fmt.Errorf("store: list diag runs: %w", err)
	}
	defer rows.Close()
	var out []diagindex.Row
	for rows.Next() {
		r, err := scanDiagRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list diag runs: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// DeleteDiagRun removes a row.
func (s *SQLite) DeleteDiagRun(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM diag_runs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete diag run: %w", err)
	}
	return nil
}

// DiagTotalSize is the sum of size_bytes over all rows.
func (s *SQLite) DiagTotalSize(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT SUM(size_bytes) FROM diag_runs`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: diag total size: %w", err)
	}
	return n.Int64, nil
}

// MarkDiagRunsInterrupted turns rows left "running" by a previous process
// into "interrupted".
func (s *SQLite) MarkDiagRunsInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE diag_runs SET status = 'interrupted', reason = 'процесс бота перезапущен во время задачи' WHERE status = 'running'`)
	if err != nil {
		return 0, fmt.Errorf("store: mark diag runs interrupted: %w", err)
	}
	return res.RowsAffected()
}

// DiagRunSummary counts the rows started at or after since by status and
// reads the start of the newest row of the whole index.
func (s *SQLite) DiagRunSummary(ctx context.Context, since time.Time) (diagindex.Summary, error) {
	sum := diagindex.Summary{ByStatus: map[string]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM diag_runs WHERE started_at >= ? GROUP BY status`, toNanos(since))
	if err != nil {
		return sum, fmt.Errorf("store: diag run summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return sum, fmt.Errorf("store: diag run summary: %w", err)
		}
		sum.ByStatus[st], sum.Total = n, sum.Total+n
	}
	if err := rows.Err(); err != nil {
		return sum, fmt.Errorf("store: diag run summary: %w", err)
	}
	var last sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(started_at) FROM diag_runs`).Scan(&last); err != nil {
		return sum, fmt.Errorf("store: diag run summary: %w", err)
	}
	sum.Last = fromNanos(last.Int64)
	return sum, nil
}
