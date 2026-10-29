package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"time"
)

// The state of each main branch for the CI failure digest (review.ci,
// internal/review/pipeline/ci_main.go): the last green pipeline (where "commits since
// green" start) and the open incident (main is red: which pipeline, which
// merged MR got the note). Named migration, registered from this file.
func init() {
	addNamedMigration("ci_main_v1", `
	CREATE TABLE IF NOT EXISTS ci_main (
		project_id     INTEGER NOT NULL,
		branch         TEXT    NOT NULL,
		green_pipeline INTEGER NOT NULL DEFAULT 0,
		green_sha      TEXT    NOT NULL DEFAULT '',
		red_pipeline   INTEGER NOT NULL DEFAULT 0,
		red_sha        TEXT    NOT NULL DEFAULT '',
		culprit_mr     INTEGER NOT NULL DEFAULT 0,
		note_id        INTEGER NOT NULL DEFAULT 0,
		last_pipeline  INTEGER NOT NULL DEFAULT 0,
		updated_at     INTEGER NOT NULL,
		PRIMARY KEY (project_id, branch)
	);`)
}

// GetCIMain returns the state of a main branch (nil when none).
func (s *SQLite) GetCIMain(ctx context.Context, projectID int64, branch string) (*core.CIMain, error) {
	var c core.CIMain
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT project_id, branch, green_pipeline, green_sha, red_pipeline, red_sha, culprit_mr, note_id,
		last_pipeline, updated_at FROM ci_main WHERE project_id = ? AND branch = ?`, projectID, branch).
		Scan(&c.ProjectID, &c.Branch, &c.GreenPipeline, &c.GreenSHA, &c.RedPipeline, &c.RedSHA, &c.CulpritMR, &c.NoteID, &c.LastPipeline, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get ci main: %w", err)
	}
	c.UpdatedAt = fromNanos(at)
	return &c, nil
}

// SaveCIMain inserts or replaces the state of a main branch.
func (s *SQLite) SaveCIMain(ctx context.Context, c core.CIMain) error {
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO ci_main(project_id, branch, green_pipeline, green_sha, red_pipeline, red_sha, culprit_mr,
		note_id, last_pipeline, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, branch) DO UPDATE SET green_pipeline = excluded.green_pipeline, green_sha = excluded.green_sha,
		red_pipeline = excluded.red_pipeline, red_sha = excluded.red_sha, culprit_mr = excluded.culprit_mr, note_id = excluded.note_id,
		last_pipeline = excluded.last_pipeline, updated_at = excluded.updated_at`,
		c.ProjectID, c.Branch, c.GreenPipeline, c.GreenSHA, c.RedPipeline, c.RedSHA, c.CulpritMR, c.NoteID, c.LastPipeline, toNanos(c.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: save ci main: %w", err)
	}
	return nil
}
