package store

// Per-project operational memory (internal/memory): one row per (project,
// section). Keyed by the numeric GitLab project ID so a renamed or moved
// repository keeps its memory; project_path is only for display and is
// refreshed on every write. The table comes from the named migration
// "project_memory_v1" (see namedMigrations in learnings.go); registering it
// from this file keeps the edit to the shared list out of other features'
// hunks.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/memory"
)

var _ memory.Store = (*SQLite)(nil)

const projectMemorySchema = `
	CREATE TABLE IF NOT EXISTS project_memory (
		project_id    INTEGER NOT NULL,
		project_path  TEXT    NOT NULL DEFAULT '',
		section       TEXT    NOT NULL,
		content       TEXT    NOT NULL,
		updated_at    INTEGER NOT NULL,
		updated_by_mr INTEGER NOT NULL DEFAULT 0,
		source_note   TEXT    NOT NULL DEFAULT '',
		PRIMARY KEY (project_id, section)
	);`

func init() {
	addNamedMigration("project_memory_v1", projectMemorySchema)
}

// ListMemory returns the project's non-empty sections in memory.Sections
// order.
func (s *SQLite) ListMemory(ctx context.Context, projectID int64) ([]memory.Section, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id, project_path, section, content, updated_at, updated_by_mr, source_note
		 FROM project_memory WHERE project_id = ? AND content <> ''`, projectID)
	if err != nil {
		return nil, fmt.Errorf("store: list memory: %w", err)
	}
	defer rows.Close()
	byKey := map[string]memory.Section{}
	for rows.Next() {
		var (
			m       memory.Section
			updated int64
		)
		if err := rows.Scan(&m.ProjectID, &m.ProjectPath, &m.Key, &m.Content, &updated, &m.UpdatedByMR, &m.SourceNote); err != nil {
			return nil, fmt.Errorf("store: scan memory: %w", err)
		}
		m.UpdatedAt = fromNanos(updated)
		byKey[m.Key] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list memory: %w", err)
	}
	var out []memory.Section
	for _, k := range memory.Sections {
		if m, ok := byKey[k]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// SaveMemorySection inserts or replaces one section (empty content deletes
// it) and refreshes the display path of all the project's rows.
func (s *SQLite) SaveMemorySection(ctx context.Context, m memory.Section) error {
	if m.ProjectID == 0 {
		return errors.New("store: memory: zero project id")
	}
	if !slices.Contains(memory.Sections, m.Key) {
		return fmt.Errorf("store: memory: unknown section %q", m.Key)
	}
	content := strings.TrimSpace(m.Content)
	if r := []rune(content); len(r) > memory.HardSectionCap {
		content = string(r[:memory.HardSectionCap])
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: save memory: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if content == "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_memory WHERE project_id = ? AND section = ?`, m.ProjectID, m.Key); err != nil {
			return fmt.Errorf("store: delete memory section: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx,
		`INSERT INTO project_memory(project_id, project_path, section, content, updated_at, updated_by_mr, source_note)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, section) DO UPDATE SET
		   project_path = excluded.project_path, content = excluded.content, updated_at = excluded.updated_at,
		   updated_by_mr = excluded.updated_by_mr, source_note = excluded.source_note`,
		m.ProjectID, m.ProjectPath, m.Key, content+"\n", toNanos(m.UpdatedAt), m.UpdatedByMR, m.SourceNote); err != nil {
		return fmt.Errorf("store: save memory section: %w", err)
	}
	if m.ProjectPath != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE project_memory SET project_path = ? WHERE project_id = ?`, m.ProjectPath, m.ProjectID); err != nil {
			return fmt.Errorf("store: memory project path: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: save memory: %w", err)
	}
	return nil
}

// ClearMemory deletes one section (key) or all (key == "").
func (s *SQLite) ClearMemory(ctx context.Context, projectID int64, key string) (int, error) {
	q, args := `DELETE FROM project_memory WHERE project_id = ?`, []any{projectID}
	if key != "" {
		q += ` AND section = ?`
		args = append(args, key)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("store: clear memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: clear memory: %w", err)
	}
	return int(n), nil
}

// SetMemoryProjectPath updates the display path of the project's rows.
func (s *SQLite) SetMemoryProjectPath(ctx context.Context, projectID int64, path string) error {
	if path == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE project_memory SET project_path = ? WHERE project_id = ? AND project_path <> ?`, path, projectID, path); err != nil {
		return fmt.Errorf("store: memory project path: %w", err)
	}
	return nil
}
