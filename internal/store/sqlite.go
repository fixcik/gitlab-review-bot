// Package store implements core.Store on top of SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // sqlite3 driver

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// SQLite is a core.Store backed by a SQLite database file.
type SQLite struct {
	db *sql.DB
}

var _ core.Store = (*SQLite)(nil)

// migrations are applied in order; index+1 is the schema version.
var migrations = []string{
	`CREATE TABLE mr_state (
		project_id      INTEGER NOT NULL,
		mr_iid          INTEGER NOT NULL,
		reviewed_sha    TEXT    NOT NULL DEFAULT '',
		summary_note_id INTEGER NOT NULL DEFAULT 0,
		updated_at      INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (project_id, mr_iid)
	);
	CREATE TABLE usage (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL DEFAULT 0,
		kind    TEXT    NOT NULL,
		mr_key  TEXT    NOT NULL DEFAULT '',
		at      INTEGER NOT NULL
	);
	CREATE INDEX usage_kind_at ON usage(kind, at);
	CREATE INDEX usage_user_kind_at ON usage(user_id, kind, at);
	CREATE INDEX usage_mr_kind_at ON usage(mr_key, kind, at);`,
}

// OpenSQLite opens (creating if needed) the database at path and applies
// pending migrations.
func OpenSQLite(path string) (*SQLite, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("store: create dir: %w", err)
			}
		}
	}
	q := url.Values{}
	q.Set("_journal_mode", "WAL")
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	q.Set("_synchronous", "NORMAL")
	q.Set("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	s := &SQLite{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrateNamed(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: schema_version: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var version int
	err = tx.QueryRowContext(ctx, `SELECT version FROM schema_version LIMIT 1`).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES (0)`); err != nil {
			return fmt.Errorf("store: init schema_version: %w", err)
		}
	case err != nil:
		return fmt.Errorf("store: read schema_version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema version %d is newer than supported %d", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, len(migrations)); err != nil {
		return fmt.Errorf("store: update schema_version: %w", err)
	}
	return tx.Commit()
}

// whereClause collects the AND-ed conditions of a filtered query with their
// arguments.
type whereClause struct {
	conds []string
	args  []any
}

func (w *whereClause) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

// sql is " WHERE c1 AND c2 ..." or "" without conditions.
func (w whereClause) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// inPlaceholders is "(?, ?, ...)" with n placeholders for an IN condition.
func inPlaceholders(n int) string { return "(?" + strings.Repeat(", ?", n-1) + ")" }

// boolInt stores a bool in an INTEGER column.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// rowScanner is *sql.Row or *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func toNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// GetMRState returns the stored state of an MR, or (nil, nil) if none.
func (s *SQLite) GetMRState(ctx context.Context, projectID, mrIID int64) (*core.MRState, error) {
	var (
		st      = core.MRState{ProjectID: projectID, MRIID: mrIID}
		updated int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT reviewed_sha, summary_note_id, updated_at FROM mr_state WHERE project_id = ? AND mr_iid = ?`,
		projectID, mrIID).Scan(&st.ReviewedSHA, &st.SummaryNoteID, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get mr state: %w", err)
	}
	st.UpdatedAt = fromNanos(updated)
	return &st, nil
}

// SaveMRState inserts or replaces the state of an MR. A zero UpdatedAt is
// replaced with the current time.
func (s *SQLite) SaveMRState(ctx context.Context, st core.MRState) error {
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mr_state(project_id, mr_iid, reviewed_sha, summary_note_id, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, mr_iid) DO UPDATE SET
		   reviewed_sha = excluded.reviewed_sha,
		   summary_note_id = excluded.summary_note_id,
		   updated_at = excluded.updated_at`,
		st.ProjectID, st.MRIID, st.ReviewedSHA, st.SummaryNoteID, toNanos(st.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: save mr state: %w", err)
	}
	return nil
}

// RecordUsage stores one usage record. A zero At is replaced with now.
func (s *SQLite) RecordUsage(ctx context.Context, u core.Usage) error {
	if u.At.IsZero() {
		u.At = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage(user_id, kind, mr_key, at) VALUES (?, ?, ?, ?)`,
		u.UserID, string(u.Kind), u.MRKey, toNanos(u.At))
	if err != nil {
		return fmt.Errorf("store: record usage: %w", err)
	}
	return nil
}

// ListUsage returns records matching f (all set fields must match; At >=
// Since when Since is non-zero), oldest first.
func (s *SQLite) ListUsage(ctx context.Context, f core.UsageFilter) ([]core.Usage, error) {
	var w whereClause
	if f.UserID != nil {
		w.add("user_id = ?", *f.UserID)
	}
	if f.Kind != nil {
		w.add("kind = ?", string(*f.Kind))
	}
	if f.MRKey != nil {
		w.add("mr_key = ?", *f.MRKey)
	}
	if !f.Since.IsZero() {
		w.add("at >= ?", toNanos(f.Since))
	}
	q := `SELECT user_id, kind, mr_key, at FROM usage` + w.sql() + " ORDER BY at ASC, id ASC"
	rows, err := s.db.QueryContext(ctx, q, w.args...)
	if err != nil {
		return nil, fmt.Errorf("store: list usage: %w", err)
	}
	defer rows.Close()
	var out []core.Usage
	for rows.Next() {
		var (
			u    core.Usage
			kind string
			at   int64
		)
		if err := rows.Scan(&u.UserID, &kind, &u.MRKey, &at); err != nil {
			return nil, fmt.Errorf("store: scan usage: %w", err)
		}
		u.Kind = core.UsageKind(kind)
		u.At = fromNanos(at)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list usage: %w", err)
	}
	return out, nil
}

// PruneUsage deletes usage records strictly older than before.
func (s *SQLite) PruneUsage(ctx context.Context, before time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM usage WHERE at < ?`, toNanos(before)); err != nil {
		return fmt.Errorf("store: prune usage: %w", err)
	}
	return nil
}

// Close closes the database.
func (s *SQLite) Close() error { return s.db.Close() }
