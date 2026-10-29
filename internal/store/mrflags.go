package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Per-MR flags set by commands (currently: "@bot pause" / "@bot resume").
//
// The table comes from a named migration (see namedMigrations in
// learnings.go). It is registered from this file so that the feature does
// not touch the shared migration list.
func init() {
	addNamedMigration("mr_flags_v1", `
	CREATE TABLE IF NOT EXISTS mr_flags (
		project_id INTEGER NOT NULL,
		mr_iid     INTEGER NOT NULL,
		paused     INTEGER NOT NULL DEFAULT 0,
		paused_by  TEXT    NOT NULL DEFAULT '',
		paused_at  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (project_id, mr_iid)
	);`)
}

// GetMRPause reports whether automatic reviews of the MR are paused, who
// paused them and when.
func (s *SQLite) GetMRPause(ctx context.Context, projectID, mrIID int64) (paused bool, by string, at time.Time, err error) {
	var p, n int64
	err = s.db.QueryRowContext(ctx,
		`SELECT paused, paused_by, paused_at FROM mr_flags WHERE project_id = ? AND mr_iid = ?`,
		projectID, mrIID).Scan(&p, &by, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", time.Time{}, nil
	}
	if err != nil {
		return false, "", time.Time{}, fmt.Errorf("store: get mr pause: %w", err)
	}
	if p == 0 {
		return false, "", time.Time{}, nil
	}
	return true, by, fromNanos(n), nil
}

// SetMRPause pauses automatic reviews of the MR (overwriting who and when).
// A zero at is replaced with the current time.
func (s *SQLite) SetMRPause(ctx context.Context, projectID, mrIID int64, by string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mr_flags(project_id, mr_iid, paused, paused_by, paused_at) VALUES (?, ?, 1, ?, ?)
		 ON CONFLICT(project_id, mr_iid) DO UPDATE SET
		   paused = 1, paused_by = excluded.paused_by, paused_at = excluded.paused_at`,
		projectID, mrIID, by, toNanos(at))
	if err != nil {
		return fmt.Errorf("store: set mr pause: %w", err)
	}
	return nil
}

// ClearMRPause resumes automatic reviews of the MR. It reports whether the
// MR was paused.
func (s *SQLite) ClearMRPause(ctx context.Context, projectID, mrIID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE mr_flags SET paused = 0, paused_by = '', paused_at = 0 WHERE project_id = ? AND mr_iid = ? AND paused = 1`,
		projectID, mrIID)
	if err != nil {
		return false, fmt.Errorf("store: clear mr pause: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: clear mr pause: %w", err)
	}
	return n > 0, nil
}
