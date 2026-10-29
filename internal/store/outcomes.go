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

var _ learn.OutcomeStore = (*SQLite)(nil)

// The outcomes tables come from their own named migration, registered here
// (not in the namedMigrations literal) so that other features can add their
// steps without touching the same lines.
func init() {
	addNamedMigration("finding_outcomes_v1", `
	CREATE TABLE IF NOT EXISTS finding_outcomes (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		project_path     TEXT    NOT NULL,
		mr_iid           INTEGER NOT NULL,
		finding_id       TEXT    NOT NULL,
		title            TEXT    NOT NULL DEFAULT '',
		severity         TEXT    NOT NULL DEFAULT '',
		path             TEXT    NOT NULL DEFAULT '',
		line             INTEGER NOT NULL DEFAULT 0,
		body_excerpt     TEXT    NOT NULL DEFAULT '',
		head_sha_at_post TEXT    NOT NULL DEFAULT '',
		status           TEXT    NOT NULL DEFAULT 'open',
		discussion_id    TEXT    NOT NULL DEFAULT '',
		rule_id          INTEGER NOT NULL DEFAULT 0,
		used_at          INTEGER NULL,
		created_at       INTEGER NOT NULL,
		updated_at       INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS finding_outcomes_mr ON finding_outcomes(project_path, mr_iid, status);
	CREATE INDEX IF NOT EXISTS finding_outcomes_project ON finding_outcomes(project_path, status, created_at);
	CREATE TABLE IF NOT EXISTS outcome_rules (
		rule_id      INTEGER PRIMARY KEY,
		project_path TEXT    NOT NULL,
		mr_iid       INTEGER NOT NULL,
		dismissed    INTEGER NOT NULL DEFAULT 0,
		shown        INTEGER NOT NULL DEFAULT 0,
		created_at   INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS outcome_rules_mr ON outcome_rules(project_path, mr_iid);`)
}

const outcomeCols = `id, project_path, mr_iid, finding_id, title, severity, path, line, body_excerpt,
	head_sha_at_post, status, discussion_id, rule_id, used_at, created_at, updated_at`

func scanOutcome(sc rowScanner) (learn.Outcome, error) {
	var (
		o                learn.Outcome
		used             sql.NullInt64
		created, updated int64
	)
	if err := sc.Scan(&o.ID, &o.ProjectPath, &o.MRIID, &o.FindingID, &o.Title, &o.Severity, &o.Path, &o.Line, &o.BodyExcerpt,
		&o.HeadSHAAtPost, &o.Status, &o.DiscussionID, &o.RuleID, &used, &created, &updated); err != nil {
		return learn.Outcome{}, err
	}
	if used.Valid {
		o.UsedAt = fromNanos(used.Int64)
	}
	o.CreatedAt, o.UpdatedAt = fromNanos(created), fromNanos(updated)
	return o, nil
}

func validOutcomeStatus(s string) bool {
	switch s {
	case learn.OutcomeOpen, learn.OutcomeFixed, learn.OutcomeDismissed, learn.OutcomeResolvedByBot, learn.OutcomeClosed,
		learn.OutcomeRejectedByVerifier:
		return true
	}
	return false
}

// RecordOutcome inserts a finding outcome.
func (s *SQLite) RecordOutcome(ctx context.Context, o learn.Outcome) (int64, error) {
	if o.Status == "" {
		o.Status = learn.OutcomeOpen
	}
	if !validOutcomeStatus(o.Status) {
		return 0, fmt.Errorf("store: invalid outcome status %q", o.Status)
	}
	if o.ProjectPath == "" || o.FindingID == "" {
		return 0, errors.New("store: outcome needs project and finding id")
	}
	now := o.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO finding_outcomes(project_path, mr_iid, finding_id, title, severity, path, line, body_excerpt,
		   head_sha_at_post, status, discussion_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ProjectPath, o.MRIID, o.FindingID, o.Title, o.Severity, o.Path, o.Line, o.BodyExcerpt,
		o.HeadSHAAtPost, o.Status, o.DiscussionID, toNanos(now), toNanos(now))
	if err != nil {
		return 0, fmt.Errorf("store: record outcome: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: record outcome: %w", err)
	}
	return id, nil
}

// ListOutcomes returns matching outcomes, newest first.
func (s *SQLite) ListOutcomes(ctx context.Context, f learn.OutcomeFilter) ([]learn.Outcome, error) {
	var w whereClause
	if f.ProjectPath != "" {
		w.add("lower(project_path) = lower(?)", f.ProjectPath)
	}
	if f.MRIID != 0 {
		w.add("mr_iid = ?", f.MRIID)
	}
	if f.Status != "" {
		w.add("status = ?", f.Status)
	}
	if f.Unused {
		w.add("used_at IS NULL")
	}
	q := `SELECT ` + outcomeCols + ` FROM finding_outcomes` + w.sql() + " ORDER BY updated_at DESC, id DESC"
	args := w.args
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list outcomes: %w", err)
	}
	defer rows.Close()
	var out []learn.Outcome
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan outcome: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list outcomes: %w", err)
	}
	return out, nil
}

// SetOutcomeDiscussion binds an outcome to its discussion.
func (s *SQLite) SetOutcomeDiscussion(ctx context.Context, id int64, discussionID string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE finding_outcomes SET discussion_id = ?, updated_at = ? WHERE id = ?`,
		discussionID, toNanos(time.Now()), id); err != nil {
		return fmt.Errorf("store: set outcome discussion: %w", err)
	}
	return nil
}

// SetOutcomeStatus changes an outcome's status.
func (s *SQLite) SetOutcomeStatus(ctx context.Context, id int64, status string) error {
	if !validOutcomeStatus(status) {
		return fmt.Errorf("store: invalid outcome status %q", status)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE finding_outcomes SET status = ?, updated_at = ? WHERE id = ?`,
		status, toNanos(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: set outcome status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: outcome %d: %w", id, sql.ErrNoRows)
	}
	return nil
}

// CloseOpenOutcome sets status on the open outcome of a thread.
func (s *SQLite) CloseOpenOutcome(ctx context.Context, projectPath string, mrIID int64, discussionID, findingID, status string) (bool, error) {
	if !validOutcomeStatus(status) {
		return false, fmt.Errorf("store: invalid outcome status %q", status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE finding_outcomes SET status = ?, updated_at = ?
		 WHERE id = (SELECT id FROM finding_outcomes
		             WHERE lower(project_path) = lower(?) AND mr_iid = ? AND status = 'open'
		               AND ((? <> '' AND discussion_id = ?) OR (discussion_id = '' AND ? <> '' AND finding_id = ?))
		             ORDER BY id DESC LIMIT 1)`,
		status, toNanos(time.Now()), projectPath, mrIID, discussionID, discussionID, findingID, findingID)
	if err != nil {
		return false, fmt.Errorf("store: close outcome: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkOutcomesUsed records that the outcomes went into a synthesis run.
func (s *SQLite) MarkOutcomesUsed(ctx context.Context, ids []int64, ruleID int64) error {
	if len(ids) == 0 {
		return nil
	}
	now := toNanos(time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: mark outcomes used: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE finding_outcomes SET used_at = ?, rule_id = ? WHERE id = ?`, now, ruleID, id); err != nil {
			return fmt.Errorf("store: mark outcomes used: %w", err)
		}
	}
	return tx.Commit()
}

// OutcomeStats counts the project's outcomes created at or after since.
func (s *SQLite) OutcomeStats(ctx context.Context, projectPath string, since time.Time) (learn.OutcomeStats, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, count(*) FROM finding_outcomes
		 WHERE lower(project_path) = lower(?) AND created_at >= ? GROUP BY status`, projectPath, toNanos(since))
	if err != nil {
		return learn.OutcomeStats{}, fmt.Errorf("store: outcome stats: %w", err)
	}
	defer rows.Close()
	var st learn.OutcomeStats
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return learn.OutcomeStats{}, fmt.Errorf("store: outcome stats: %w", err)
		}
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
	}
	if err := rows.Err(); err != nil {
		return learn.OutcomeStats{}, fmt.Errorf("store: outcome stats: %w", err)
	}
	return st, nil
}

// AddSynthRule remembers a synthesized rule (replacing an earlier entry of
// the same rule).
func (s *SQLite) AddSynthRule(ctx context.Context, r learn.SynthRule) error {
	now := r.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO outcome_rules(rule_id, project_path, mr_iid, dismissed, shown, created_at) VALUES (?, ?, ?, ?, 0, ?)
		 ON CONFLICT(rule_id) DO UPDATE SET project_path = excluded.project_path, mr_iid = excluded.mr_iid,
		   dismissed = excluded.dismissed, shown = 0, created_at = excluded.created_at`,
		r.RuleID, r.ProjectPath, r.MRIID, r.Dismissed, toNanos(now)); err != nil {
		return fmt.Errorf("store: add synth rule: %w", err)
	}
	return nil
}

// PendingSynthRules returns an MR's synthesized rules shown < maxShown times.
func (s *SQLite) PendingSynthRules(ctx context.Context, projectPath string, mrIID int64, maxShown int) ([]learn.SynthRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rule_id, project_path, mr_iid, dismissed, shown, created_at FROM outcome_rules
		 WHERE lower(project_path) = lower(?) AND mr_iid = ? AND shown < ? ORDER BY created_at, rule_id`,
		projectPath, mrIID, maxShown)
	if err != nil {
		return nil, fmt.Errorf("store: pending synth rules: %w", err)
	}
	defer rows.Close()
	var out []learn.SynthRule
	for rows.Next() {
		var (
			r       learn.SynthRule
			created int64
		)
		if err := rows.Scan(&r.RuleID, &r.ProjectPath, &r.MRIID, &r.Dismissed, &r.Shown, &created); err != nil {
			return nil, fmt.Errorf("store: pending synth rules: %w", err)
		}
		r.CreatedAt = fromNanos(created)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pending synth rules: %w", err)
	}
	return out, nil
}

// MarkSynthRulesShown increments the shown counters.
func (s *SQLite) MarkSynthRulesShown(ctx context.Context, ruleIDs []int64) error {
	for _, id := range ruleIDs {
		if _, err := s.db.ExecContext(ctx, `UPDATE outcome_rules SET shown = shown + 1 WHERE rule_id = ?`, id); err != nil {
			return fmt.Errorf("store: mark synth rules shown: %w", err)
		}
	}
	return nil
}

// SynthRuleSources maps synthesized rule ids to their dismissed counts.
func (s *SQLite) SynthRuleSources(ctx context.Context, ruleIDs []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(ruleIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ruleIDs)), ",")
	args := make([]any, len(ruleIDs))
	for i, id := range ruleIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rule_id, dismissed FROM outcome_rules WHERE rule_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: synth rule sources: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: synth rule sources: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// TouchOutcomes bumps updated_at of the MR's open outcomes.
func (s *SQLite) TouchOutcomes(ctx context.Context, projectPath string, mrIID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE finding_outcomes SET updated_at = ? WHERE lower(project_path) = lower(?) AND mr_iid = ? AND status = 'open'`,
		toNanos(time.Now()), projectPath, mrIID); err != nil {
		return fmt.Errorf("store: touch outcomes: %w", err)
	}
	return nil
}
