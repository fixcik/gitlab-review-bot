package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Labels the bot manages on an MR (review.labels): which labels it added
// itself (only those may be removed as stale) and which of them a human took
// off afterwards (never set again on that MR). Named migration, registered
// from this file.
func init() {
	addNamedMigration("mr_labels_v1", `
	CREATE TABLE IF NOT EXISTS mr_labels (
		project_id INTEGER NOT NULL,
		mr_iid     INTEGER NOT NULL,
		label      TEXT    NOT NULL,
		state      TEXT    NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (project_id, mr_iid, label)
	);`)
}

// Label states in mr_labels.
const (
	labelAdded      = "added"
	labelSuppressed = "suppressed"
)

// BotLabels returns the labels the bot added to the MR and the ones a human
// removed after the bot added them, both sorted.
func (s *SQLite) BotLabels(ctx context.Context, projectID, mrIID int64) (added, suppressed []string, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT label, state FROM mr_labels WHERE project_id = ? AND mr_iid = ? ORDER BY label`, projectID, mrIID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: bot labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l, st string
		if err := rows.Scan(&l, &st); err != nil {
			return nil, nil, fmt.Errorf("store: bot labels: %w", err)
		}
		if st == labelSuppressed {
			suppressed = append(suppressed, l)
		} else {
			added = append(added, l)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: bot labels: %w", err)
	}
	return added, suppressed, nil
}

// SaveBotLabels replaces the MR's bot label state (one transaction). A
// label in both lists is stored as suppressed.
func (s *SQLite) SaveBotLabels(ctx context.Context, projectID, mrIID int64, added, suppressed []string) error {
	state := map[string]string{}
	for _, l := range added {
		state[l] = labelAdded
	}
	for _, l := range suppressed {
		state[l] = labelSuppressed
	}
	names := make([]string, 0, len(state))
	for l := range state {
		names = append(names, l)
	}
	sort.Strings(names)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: save bot labels: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, `DELETE FROM mr_labels WHERE project_id = ? AND mr_iid = ?`, projectID, mrIID); err != nil {
		return fmt.Errorf("store: save bot labels: %w", err)
	}
	now := toNanos(time.Now())
	for _, l := range names {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO mr_labels(project_id, mr_iid, label, state, updated_at) VALUES (?, ?, ?, ?, ?)`,
			projectID, mrIID, l, state[l], now); err != nil {
			return fmt.Errorf("store: save bot labels: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: save bot labels: %w", err)
	}
	return nil
}
