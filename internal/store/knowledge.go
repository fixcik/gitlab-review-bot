package store

// Read-side queries of the /knowledge page (internal/knowledgeweb): everything
// the bot has learned, across all projects.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
)

// ListAllLearnings returns every live rule (global and of every project),
// newest first, at most limit (<= 0: all).
func (s *SQLite) ListAllLearnings(ctx context.Context, limit int) ([]learn.Rule, error) {
	q := `SELECT ` + learningCols + ` FROM learnings WHERE deleted_at IS NULL ORDER BY updated_at DESC, id DESC`
	var args []any
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list all learnings: %w", err)
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
		return nil, fmt.Errorf("store: list all learnings: %w", err)
	}
	return out, nil
}

// ListAllMemory returns the non-empty memory sections of every project,
// ordered by project path, then memory.Sections order.
func (s *SQLite) ListAllMemory(ctx context.Context) ([]memory.Section, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id, project_path, section, content, updated_at, updated_by_mr, source_note
		 FROM project_memory WHERE content <> ''`)
	if err != nil {
		return nil, fmt.Errorf("store: list all memory: %w", err)
	}
	defer rows.Close()
	var out []memory.Section
	for rows.Next() {
		var (
			m       memory.Section
			updated int64
		)
		if err := rows.Scan(&m.ProjectID, &m.ProjectPath, &m.Key, &m.Content, &updated, &m.UpdatedByMR, &m.SourceNote); err != nil {
			return nil, fmt.Errorf("store: scan memory: %w", err)
		}
		m.UpdatedAt = fromNanos(updated)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list all memory: %w", err)
	}
	slices.SortFunc(out, func(a, b memory.Section) int {
		if a.ProjectPath != b.ProjectPath {
			if a.ProjectPath < b.ProjectPath {
				return -1
			}
			return 1
		}
		if a.ProjectID != b.ProjectID {
			if a.ProjectID < b.ProjectID {
				return -1
			}
			return 1
		}
		return slices.Index(memory.Sections, a.Key) - slices.Index(memory.Sections, b.Key)
	})
	return out, nil
}

// OutcomeStatsAll counts the outcomes created since the given time for every
// project at once, keyed by the lower-cased project path (GitLab paths are
// case-insensitive).
func (s *SQLite) OutcomeStatsAll(ctx context.Context, since time.Time) (map[string]learn.OutcomeStats, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT lower(project_path), status, count(*) FROM finding_outcomes
		 WHERE created_at >= ? GROUP BY lower(project_path), status`, toNanos(since))
	if err != nil {
		return nil, fmt.Errorf("store: outcome stats all: %w", err)
	}
	defer rows.Close()
	out := map[string]learn.OutcomeStats{}
	for rows.Next() {
		var (
			path, status string
			n            int
		)
		if err := rows.Scan(&path, &status, &n); err != nil {
			return nil, fmt.Errorf("store: outcome stats all: %w", err)
		}
		st := out[path]
		switch status {
		case learn.OutcomeOpen:
			st.Open = n
		case learn.OutcomeFixed:
			st.Fixed = n
		case learn.OutcomeDismissed:
			st.Dismissed = n
		case learn.OutcomeResolvedByBot:
			st.ResolvedByBot = n
		case learn.OutcomeClosed:
			st.Closed = n
		case learn.OutcomeRejectedByVerifier:
			st.RejectedByVerifier = n
		}
		out[path] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: outcome stats all: %w", err)
	}
	return out, nil
}

// ListDeletedLearnings returns the soft-deleted rules, most recently deleted
// first, at most limit (<= 0: all).
func (s *SQLite) ListDeletedLearnings(ctx context.Context, limit int) ([]learn.Rule, error) {
	q := `SELECT ` + learningCols + `, deleted_at FROM learnings WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC, id DESC`
	var args []any
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list deleted learnings: %w", err)
	}
	defer rows.Close()
	var out []learn.Rule
	for rows.Next() {
		var (
			r                         learn.Rule
			created, updated, deleted int64
		)
		if err := rows.Scan(&r.ID, &r.Scope, &r.ProjectPath, &r.Text, &r.SourceURL, &r.SourceNoteID, &r.Author, &created, &updated, &deleted); err != nil {
			return nil, fmt.Errorf("store: scan learning: %w", err)
		}
		r.CreatedAt, r.UpdatedAt, r.DeletedAt = fromNanos(created), fromNanos(updated), fromNanos(deleted)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list deleted learnings: %w", err)
	}
	return out, nil
}

// inTx runs fn in a transaction (committed when fn returns nil).
func (s *SQLite) inTx(ctx context.Context, what string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	return nil
}

// liveProject returns the project path of a live rule (learn.ErrRuleNotFound
// when id is not one).
func liveProject(ctx context.Context, tx *sql.Tx, id int64) (string, error) {
	var p string
	err := tx.QueryRowContext(ctx, `SELECT project_path FROM learnings WHERE id = ? AND deleted_at IS NULL`, id).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: learning %d: %w", id, learn.ErrRuleNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("store: read learning %d: %w", id, err)
	}
	return p, nil
}

// DeleteLearnings soft-deletes the rules, all or nothing: when one id is not
// a live rule nothing is deleted (learn.ErrRuleNotFound).
func (s *SQLite) DeleteLearnings(ctx context.Context, ids []int64) error {
	now := toNanos(time.Now())
	return s.inTx(ctx, "delete learnings", func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := liveProject(ctx, tx, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE learnings SET deleted_at = ? WHERE id = ?`, now, id); err != nil {
				return fmt.Errorf("store: delete learning %d: %w", id, err)
			}
		}
		return nil
	})
}

// SetLearningsScope makes the live rules global or project-only, keeping
// their project path, all or nothing: learn.ErrRuleNotFound when an id is
// not a live rule, learn.ErrRuleNoProject when a rule to be limited to its
// project does not remember one.
func (s *SQLite) SetLearningsScope(ctx context.Context, ids []int64, scope string) error {
	scope = learn.NormalizeScope(scope)
	now := toNanos(time.Now())
	return s.inTx(ctx, "set learnings scope", func(tx *sql.Tx) error {
		for _, id := range ids {
			p, err := liveProject(ctx, tx, id)
			if err != nil {
				return err
			}
			if scope == learn.ScopeProject && p == "" {
				return fmt.Errorf("store: learning %d: %w", id, learn.ErrRuleNoProject)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE learnings SET scope = ?, updated_at = ? WHERE id = ?`, scope, now, id); err != nil {
				return fmt.Errorf("store: set learning scope %d: %w", id, err)
			}
		}
		return nil
	})
}

// UndeleteLearnings restores soft-deleted rules (clears deleted_at; the
// other fields, updated_at included, stay as they were), all or nothing.
// It refuses, like the de-duplication of AddLearning, a rule equal to a
// live one (same normalized text and scope, same project for project
// scope): conflicts maps such a deleted id to the live rule's id and nothing
// is restored. An id that is not a deleted rule gives learn.ErrRuleNotFound.
// Rules are restored in order, so of two equal deleted rules in one call
// the second conflicts with the first.
func (s *SQLite) UndeleteLearnings(ctx context.Context, ids []int64) (conflicts map[int64]int64, err error) {
	conflicts = map[int64]int64{}
	errConflict := errors.New("conflict")
	err = s.inTx(ctx, "undelete learnings", func(tx *sql.Tx) error {
		for _, id := range ids {
			var key, scope, project string
			err := tx.QueryRowContext(ctx, `SELECT rule_key, scope, project_path FROM learnings WHERE id = ? AND deleted_at IS NOT NULL`, id).
				Scan(&key, &scope, &project)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("store: deleted learning %d: %w", id, learn.ErrRuleNotFound)
			}
			if err != nil {
				return fmt.Errorf("store: read learning %d: %w", id, err)
			}
			var live int64
			err = tx.QueryRowContext(ctx,
				`SELECT id FROM learnings
				 WHERE deleted_at IS NULL AND rule_key = ? AND scope = ?
				   AND (scope = 'global' OR lower(project_path) = lower(?))
				 ORDER BY id LIMIT 1`, key, scope, project).Scan(&live)
			switch {
			case err == nil:
				conflicts[id] = live
				continue
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("store: find learning: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE learnings SET deleted_at = NULL WHERE id = ?`, id); err != nil {
				return fmt.Errorf("store: undelete learning %d: %w", id, err)
			}
		}
		if len(conflicts) > 0 {
			return errConflict // roll back the ones restored before
		}
		return nil
	})
	if errors.Is(err, errConflict) {
		return conflicts, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, nil
}

// UndeleteLearning restores one soft-deleted rule: learn.ErrRuleExists when
// an equal live rule exists (nothing is changed), learn.ErrRuleNotFound
// when id is not a deleted rule.
func (s *SQLite) UndeleteLearning(ctx context.Context, id int64) error {
	conflicts, err := s.UndeleteLearnings(ctx, []int64{id})
	if err != nil {
		return err
	}
	if live, ok := conflicts[id]; ok {
		return fmt.Errorf("store: learning %d equals live learning %d: %w", id, live, learn.ErrRuleExists)
	}
	return nil
}

// decidedStatuses are the outcomes with a verdict on the finding.
var decidedStatuses = []any{learn.OutcomeFixed, learn.OutcomeDismissed, learn.OutcomeRejectedByVerifier}

// decisionWhere: the window of OutcomeStatsAll (created_at >= since,
// project case-insensitive) restricted to the given statuses.
func decisionWhere(project string, since time.Time, statuses []any) whereClause {
	var w whereClause
	w.add("status IN "+inPlaceholders(len(statuses)), statuses...)
	w.add("created_at >= ?", toNanos(since))
	if project != "" {
		w.add("lower(project_path) = lower(?)", project)
	}
	return w
}

// ListDecisions returns the decided outcomes (fixed, dismissed,
// rejected_by_verifier) of the filter's window, newest decision first
// (updated_at is set when the status changes), a page of Limit from Offset.
func (s *SQLite) ListDecisions(ctx context.Context, f learn.DecisionFilter) ([]learn.Outcome, error) {
	statuses := decidedStatuses
	if f.Status != "" {
		statuses = []any{f.Status}
	}
	w := decisionWhere(f.ProjectPath, f.Since, statuses)
	q := `SELECT ` + outcomeCols + ` FROM finding_outcomes` + w.sql() + ` ORDER BY updated_at DESC, id DESC`
	args := w.args
	if f.Limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, f.Limit, max(f.Offset, 0))
	}
	return s.queryOutcomes(ctx, "list decisions", q, args)
}

// DecisionSeries returns the fixed and dismissed outcomes of the window
// (the terms of the precision) in the order they were decided, oldest
// first, at most limit (<= 0: all), for the accumulated precision chart.
func (s *SQLite) DecisionSeries(ctx context.Context, project string, since time.Time, limit int) ([]learn.Outcome, error) {
	w := decisionWhere(project, since, []any{learn.OutcomeFixed, learn.OutcomeDismissed})
	q := `SELECT ` + outcomeCols + ` FROM finding_outcomes` + w.sql() + ` ORDER BY updated_at, id`
	args := w.args
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return s.queryOutcomes(ctx, "decision series", q, args)
}

func (s *SQLite) queryOutcomes(ctx context.Context, what, q string, args []any) ([]learn.Outcome, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	defer rows.Close()
	var out []learn.Outcome
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			return nil, fmt.Errorf("store: %s: %w", what, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	return out, nil
}
