package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/learn"
)

var _ learn.Store = (*SQLite)(nil)

// namedMigrations are applied after the sequential schema_version
// migrations. Each one is keyed by a unique name recorded in
// schema_migrations and is written to be idempotent (IF NOT EXISTS), so
// feature branches can add their own steps without competing for the next
// sequential version number, and a database that already has a step (or
// the objects it creates) is left alone. SQLite has no ADD COLUMN IF NOT
// EXISTS: a step that is a single "ALTER TABLE ... ADD COLUMN" statement
// counts as applied when the column is already there (see addsColumn).
var namedMigrations = []namedMigration{
	{"learnings_v1", `
	CREATE TABLE IF NOT EXISTS learnings (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		scope          TEXT    NOT NULL,
		project_path   TEXT    NOT NULL DEFAULT '',
		rule           TEXT    NOT NULL,
		rule_key       TEXT    NOT NULL,
		source_url     TEXT    NOT NULL DEFAULT '',
		source_note_id INTEGER NOT NULL DEFAULT 0,
		author         TEXT    NOT NULL DEFAULT '',
		created_at     INTEGER NOT NULL,
		updated_at     INTEGER NOT NULL,
		deleted_at     INTEGER NULL
	);
	CREATE INDEX IF NOT EXISTS learnings_live ON learnings(deleted_at, scope, project_path);
	CREATE INDEX IF NOT EXISTS learnings_key ON learnings(rule_key, scope);
	CREATE TABLE IF NOT EXISTS learn_processed_notes (
		note_id INTEGER PRIMARY KEY,
		at      INTEGER NOT NULL
	);`},
}

type namedMigration struct {
	name string
	sql  string
}

// addNamedMigration registers a named migration (from an init function of
// the feature's file, so files do not compete for one list).
func addNamedMigration(name, sql string) {
	namedMigrations = append(namedMigrations, namedMigration{name, sql})
}

// migrateNamed applies pending namedMigrations in one transaction.
func (s *SQLite) migrateNamed(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin named migrations: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: schema_migrations: %w", err)
	}
	for _, m := range namedMigrations {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE name = ?`, m.name).Scan(&one)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: read schema_migrations: %w", err)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil && !(addsColumn(m.sql) && strings.Contains(err.Error(), "duplicate column name")) {
			return fmt.Errorf("store: migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(name, applied_at) VALUES (?, ?)`, m.name, time.Now().UTC().UnixNano()); err != nil {
			return fmt.Errorf("store: record migration %s: %w", m.name, err)
		}
	}
	return tx.Commit()
}

// addsColumn reports whether a migration is one ALTER TABLE ... ADD COLUMN
// statement (so "duplicate column name" means it is already applied).
func addsColumn(q string) bool {
	q = strings.TrimSuffix(strings.TrimSpace(q), ";")
	u := strings.ToUpper(q)
	return strings.HasPrefix(u, "ALTER TABLE ") && strings.Contains(u, " ADD COLUMN ") && !strings.Contains(q, ";")
}

const learningCols = `id, scope, project_path, rule, source_url, source_note_id, author, created_at, updated_at`

func scanLearning(sc rowScanner) (learn.Rule, error) {
	var (
		r                learn.Rule
		created, updated int64
	)
	if err := sc.Scan(&r.ID, &r.Scope, &r.ProjectPath, &r.Text, &r.SourceURL, &r.SourceNoteID, &r.Author, &created, &updated); err != nil {
		return learn.Rule{}, err
	}
	r.CreatedAt, r.UpdatedAt = fromNanos(created), fromNanos(updated)
	return r, nil
}

// AddLearning inserts a rule or refreshes an equal live one (same
// normalized text and scope; same project for project-scoped rules).
func (s *SQLite) AddLearning(ctx context.Context, r learn.Rule) (learn.Rule, bool, error) {
	r.Scope = learn.NormalizeScope(r.Scope)
	r.Text = strings.TrimSpace(r.Text)
	key := learn.NormalizeKey(r.Text)
	if key == "" {
		return learn.Rule{}, false, errors.New("store: empty rule")
	}
	now := r.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return learn.Rule{}, false, fmt.Errorf("store: add learning: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM learnings
		 WHERE deleted_at IS NULL AND rule_key = ? AND scope = ?
		   AND (scope = 'global' OR lower(project_path) = lower(?))
		 ORDER BY id LIMIT 1`, key, r.Scope, r.ProjectPath).Scan(&id)
	created := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx,
			`INSERT INTO learnings(scope, project_path, rule, rule_key, source_url, source_note_id, author, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Scope, r.ProjectPath, r.Text, key, r.SourceURL, r.SourceNoteID, r.Author, toNanos(now), toNanos(now))
		if err != nil {
			return learn.Rule{}, false, fmt.Errorf("store: insert learning: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return learn.Rule{}, false, fmt.Errorf("store: insert learning: %w", err)
		}
		created = true
	case err != nil:
		return learn.Rule{}, false, fmt.Errorf("store: find learning: %w", err)
	default:
		if _, err := tx.ExecContext(ctx,
			`UPDATE learnings SET rule = ?, source_url = ?, source_note_id = ?, author = ?, updated_at = ? WHERE id = ?`,
			r.Text, r.SourceURL, r.SourceNoteID, r.Author, toNanos(now), id); err != nil {
			return learn.Rule{}, false, fmt.Errorf("store: update learning: %w", err)
		}
	}
	saved, err := scanLearning(tx.QueryRowContext(ctx, `SELECT `+learningCols+` FROM learnings WHERE id = ?`, id))
	if err != nil {
		return learn.Rule{}, false, fmt.Errorf("store: read learning: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return learn.Rule{}, false, fmt.Errorf("store: add learning: %w", err)
	}
	return saved, created, nil
}

// GetLearning returns a live rule or (nil, nil).
func (s *SQLite) GetLearning(ctx context.Context, id int64) (*learn.Rule, error) {
	r, err := scanLearning(s.db.QueryRowContext(ctx,
		`SELECT `+learningCols+` FROM learnings WHERE id = ? AND deleted_at IS NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get learning: %w", err)
	}
	return &r, nil
}

// ListLearnings returns live global rules plus the project's own, newest
// (most recently created or re-confirmed) first.
func (s *SQLite) ListLearnings(ctx context.Context, projectPath string, limit int) ([]learn.Rule, error) {
	q := `SELECT ` + learningCols + ` FROM learnings
	      WHERE deleted_at IS NULL AND (scope = 'global' OR lower(project_path) = lower(?))
	      ORDER BY updated_at DESC, id DESC`
	args := []any{projectPath}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list learnings: %w", err)
	}
	defer rows.Close()
	var out []learn.Rule
	for rows.Next() {
		r, err := scanLearning(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan learning: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list learnings: %w", err)
	}
	return out, nil
}

// SetLearningScope changes the scope of a live rule. projectPath becomes the
// rule's project (for global rules: where it came from).
func (s *SQLite) SetLearningScope(ctx context.Context, id int64, scope, projectPath string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE learnings SET scope = ?, project_path = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		learn.NormalizeScope(scope), projectPath, toNanos(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: set learning scope: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: learning %d: %w", id, sql.ErrNoRows)
	}
	return nil
}

// DeleteLearning soft-deletes a rule (no-op for an unknown or deleted one).
func (s *SQLite) DeleteLearning(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE learnings SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, toNanos(time.Now()), id); err != nil {
		return fmt.Errorf("store: delete learning: %w", err)
	}
	return nil
}

// MarkNoteProcessed records noteID; false when it was already recorded.
func (s *SQLite) MarkNoteProcessed(ctx context.Context, noteID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO learn_processed_notes(note_id, at) VALUES (?, ?)`, noteID, toNanos(time.Now()))
	if err != nil {
		return false, fmt.Errorf("store: mark note processed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: mark note processed: %w", err)
	}
	return n == 1, nil
}
