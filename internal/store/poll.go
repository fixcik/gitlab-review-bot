package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/events"
)

// Polling state (gitlab.events: poll | both): one cursor per polled group
// ("" = all visible MRs) and the last seen state of every MR.
func init() {
	addNamedMigration("poll_v1", `
	CREATE TABLE IF NOT EXISTS poll_cursors (
		scope  TEXT    PRIMARY KEY,
		cursor INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS poll_mrs (
		project_id   INTEGER NOT NULL,
		mr_iid       INTEGER NOT NULL,
		state        TEXT    NOT NULL,
		draft        INTEGER NOT NULL,
		sha          TEXT    NOT NULL,
		title        TEXT    NOT NULL,
		updated_at   INTEGER NOT NULL,
		last_note_id INTEGER NOT NULL,
		notes_since  INTEGER NOT NULL,
		PRIMARY KEY (project_id, mr_iid)
	);`)
}

var _ events.PollState = (*SQLite)(nil)

// GetPollCursor returns the cursor of a polled scope; ok is false before the
// first poll.
func (s *SQLite) GetPollCursor(ctx context.Context, scope string) (time.Time, bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT cursor FROM poll_cursors WHERE scope = ?`, scope).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: get poll cursor: %w", err)
	}
	return fromNanos(n), true, nil
}

// SetPollCursor stores the cursor of a polled scope.
func (s *SQLite) SetPollCursor(ctx context.Context, scope string, t time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO poll_cursors(scope, cursor) VALUES (?, ?)
		 ON CONFLICT(scope) DO UPDATE SET cursor = excluded.cursor`, scope, toNanos(t))
	if err != nil {
		return fmt.Errorf("store: set poll cursor: %w", err)
	}
	return nil
}

// GetPollMR returns the last seen state of an MR, or nil.
func (s *SQLite) GetPollMR(ctx context.Context, projectID, mrIID int64) (*events.PollMR, error) {
	r := events.PollMR{ProjectID: projectID, IID: mrIID}
	var draft int
	var updated, since int64
	err := s.db.QueryRowContext(ctx,
		`SELECT state, draft, sha, title, updated_at, last_note_id, notes_since
		 FROM poll_mrs WHERE project_id = ? AND mr_iid = ?`, projectID, mrIID).
		Scan(&r.State, &draft, &r.SHA, &r.Title, &updated, &r.LastNoteID, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get poll mr: %w", err)
	}
	r.Draft, r.UpdatedAt, r.NotesSince = draft != 0, fromNanos(updated), fromNanos(since)
	return &r, nil
}

// SavePollMR stores the last seen state of an MR.
func (s *SQLite) SavePollMR(ctx context.Context, r events.PollMR) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO poll_mrs(project_id, mr_iid, state, draft, sha, title, updated_at, last_note_id, notes_since)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, mr_iid) DO UPDATE SET
		   state = excluded.state, draft = excluded.draft, sha = excluded.sha, title = excluded.title,
		   updated_at = excluded.updated_at, last_note_id = excluded.last_note_id, notes_since = excluded.notes_since`,
		r.ProjectID, r.IID, r.State, boolInt(r.Draft), r.SHA, r.Title, toNanos(r.UpdatedAt), r.LastNoteID, toNanos(r.NotesSince))
	if err != nil {
		return fmt.Errorf("store: save poll mr: %w", err)
	}
	return nil
}
