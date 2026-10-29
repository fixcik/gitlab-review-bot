package store

// Verification problems (internal/problems): one row per fingerprint (kind
// + project + normalized subject) with first/last seen, a counter, the last
// detail, MR and diagnostics run, and the acknowledgement / notification
// times. The table comes from the named migration problems_v1 registered
// here (see namedMigrations in learnings.go).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/problems"
)

var _ problems.Store = (*SQLite)(nil)

func init() {
	addNamedMigration("problems_v1", `
	CREATE TABLE IF NOT EXISTS problems (
		fingerprint  TEXT    PRIMARY KEY,
		kind         TEXT    NOT NULL,
		source       TEXT    NOT NULL DEFAULT 'bot',
		project_path TEXT    NOT NULL DEFAULT '',
		subject      TEXT    NOT NULL DEFAULT '',
		detail       TEXT    NOT NULL DEFAULT '',
		first_seen   INTEGER NOT NULL,
		last_seen    INTEGER NOT NULL,
		count        INTEGER NOT NULL DEFAULT 1,
		last_mr_iid  INTEGER NOT NULL DEFAULT 0,
		last_run_id  TEXT    NOT NULL DEFAULT '',
		acked_at     INTEGER NOT NULL DEFAULT 0,
		notified_at  INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS problems_last_seen ON problems(last_seen);`)
	// resolved_at: the bot saw the cause gone (ResolveProblem).
	addNamedMigration("problems_v2", `ALTER TABLE problems ADD COLUMN resolved_at INTEGER NOT NULL DEFAULT 0`)
}

// UpsertProblem implements problems.Store.
func (s *SQLite) UpsertProblem(ctx context.Context, r problems.Record) (problems.Upserted, error) {
	var up problems.Upserted
	if r.Fingerprint == "" || r.Kind == "" {
		return up, errors.New("store: upsert problem: fingerprint and kind are required")
	}
	at := r.LastSeen
	if at.IsZero() {
		at = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return up, fmt.Errorf("store: upsert problem: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var acked, resolved, notified int64
	err = tx.QueryRowContext(ctx, `SELECT acked_at, resolved_at, notified_at FROM problems WHERE fingerprint = ?`, r.Fingerprint).Scan(&acked, &resolved, &notified)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		up.New = true
		_, err = tx.ExecContext(ctx, `INSERT INTO problems(fingerprint, kind, source, project_path, subject, detail,
			first_seen, last_seen, count, last_mr_iid, last_run_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			r.Fingerprint, string(r.Kind), string(r.Source), r.ProjectPath, r.Subject, r.Detail,
			toNanos(at), toNanos(at), r.LastMRIID, r.LastRunID)
	case err != nil:
	default:
		up.Reappeared = acked != 0 || resolved != 0
		if notified != 0 {
			up.NotifiedAt = fromNanos(notified)
		}
		// The subject keeps its first spelling (the fingerprint normalizes
		// it); everything else is the latest.
		_, err = tx.ExecContext(ctx, `UPDATE problems SET detail = ?, last_seen = ?, count = count + 1,
			last_mr_iid = ?, last_run_id = ?, acked_at = 0, resolved_at = 0, source = ? WHERE fingerprint = ?`,
			r.Detail, toNanos(at), r.LastMRIID, r.LastRunID, string(r.Source), r.Fingerprint)
	}
	if err != nil {
		return problems.Upserted{}, fmt.Errorf("store: upsert problem: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return problems.Upserted{}, fmt.Errorf("store: upsert problem: %w", err)
	}
	return up, nil
}

// MarkProblemsNotified implements problems.Store.
func (s *SQLite) MarkProblemsNotified(ctx context.Context, fps []string, at time.Time) error {
	if len(fps) == 0 {
		return nil
	}
	args := []any{toNanos(at)}
	for _, fp := range fps {
		args = append(args, fp)
	}
	q := `UPDATE problems SET notified_at = ? WHERE fingerprint IN ` + inPlaceholders(len(fps))
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("store: mark problems notified: %w", err)
	}
	return nil
}

// ListProblems implements problems.Store: newest last_seen first.
func (s *SQLite) ListProblems(ctx context.Context, f problems.Filter) ([]problems.Record, error) {
	q := `SELECT fingerprint, kind, source, project_path, subject, detail, first_seen, last_seen, count,
		last_mr_iid, last_run_id, acked_at, resolved_at, notified_at FROM problems WHERE 1 = 1`
	var args []any
	if f.Kind != "" {
		q += ` AND kind = ?`
		args = append(args, string(f.Kind))
	}
	if f.Project != "" {
		q += ` AND lower(project_path) = lower(?)`
		args = append(args, f.Project)
	}
	switch f.Status {
	case problems.StatusNew:
		q += ` AND acked_at = 0 AND resolved_at = 0`
	case problems.StatusAcked:
		q += ` AND acked_at != 0`
	case problems.StatusResolved:
		q += ` AND resolved_at != 0`
	}
	q += ` ORDER BY last_seen DESC, fingerprint`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list problems: %w", err)
	}
	defer rows.Close()
	var out []problems.Record
	for rows.Next() {
		var r problems.Record
		var kind, source string
		var first, last, acked, resolved, notified int64
		if err := rows.Scan(&r.Fingerprint, &kind, &source, &r.ProjectPath, &r.Subject, &r.Detail, &first, &last, &r.Count,
			&r.LastMRIID, &r.LastRunID, &acked, &resolved, &notified); err != nil {
			return nil, fmt.Errorf("store: list problems: %w", err)
		}
		r.Kind, r.Source = problems.Kind(kind), problems.Source(source)
		r.FirstSeen, r.LastSeen = fromNanos(first), fromNanos(last)
		if acked != 0 {
			r.AckedAt = fromNanos(acked)
		}
		if resolved != 0 {
			r.ResolvedAt = fromNanos(resolved)
		}
		if notified != 0 {
			r.NotifiedAt = fromNanos(notified)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list problems: %w", err)
	}
	return out, nil
}

// AckProblem implements problems.Store.
func (s *SQLite) AckProblem(ctx context.Context, fp string, at time.Time) (bool, error) {
	if at.IsZero() {
		at = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE problems SET acked_at = ? WHERE fingerprint = ? AND acked_at = 0`, toNanos(at), fp)
	if err != nil {
		return false, fmt.Errorf("store: ack problem: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: ack problem: %w", err)
	}
	return n > 0, nil
}

// UnackProblem returns a hidden problem to the active ones ("Вернуть" on
// /problems); false when there is no such acknowledged row.
func (s *SQLite) UnackProblem(ctx context.Context, fp string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE problems SET acked_at = 0 WHERE fingerprint = ? AND acked_at != 0`, fp)
	if err != nil {
		return false, fmt.Errorf("store: unack problem: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: unack problem: %w", err)
	}
	return n > 0, nil
}

// ResolveProblem implements problems.Store.
func (s *SQLite) ResolveProblem(ctx context.Context, fp string, at time.Time) (bool, error) {
	if at.IsZero() {
		at = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE problems SET resolved_at = ? WHERE fingerprint = ? AND resolved_at = 0`, toNanos(at), fp)
	if err != nil {
		return false, fmt.Errorf("store: resolve problem: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: resolve problem: %w", err)
	}
	return n > 0, nil
}

// PruneProblems implements problems.Store: rows not seen since before.
func (s *SQLite) PruneProblems(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM problems WHERE last_seen < ?`, toNanos(before))
	if err != nil {
		return 0, fmt.Errorf("store: prune problems: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune problems: %w", err)
	}
	return n, nil
}
