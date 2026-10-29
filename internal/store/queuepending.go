package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
)

// Pending automatic reviews of the queue (queue.Persister): one row per MR,
// so a restart does not lose debounced or postponed (RetryLater) reviews.
// Every other job is stored one row per job in queue_jobs (queuejobs.go).
func init() {
	addNamedMigration("queue_pending_v1", `
	CREATE TABLE IF NOT EXISTS queue_pending (
		project_id INTEGER NOT NULL,
		mr_iid     INTEGER NOT NULL,
		job        TEXT    NOT NULL,
		ready_at   INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (project_id, mr_iid)
	);`)
}

var _ queue.Persister = (*SQLite)(nil)

// pendingJob is the stored form of a core.Job (explicit fields, so a rename
// in core does not silently change the stored format).
type pendingJob struct {
	Kind        core.JobKind `json:"kind"`
	ProjectID   int64        `json:"project_id"`
	ProjectPath string       `json:"project_path"`
	MRIID       int64        `json:"mr_iid"`
	HeadSHA     string       `json:"head_sha,omitempty"`
	ForceFull   bool         `json:"force_full,omitempty"`
	EnqueuedAt  time.Time    `json:"enqueued_at"`
}

// SavePending stores (or replaces) the pending automatic review of an MR.
func (s *SQLite) SavePending(ctx context.Context, p queue.Pending) error {
	j := p.Job
	if j.Kind != core.JobAutoReview {
		return fmt.Errorf("store: only automatic reviews are persisted, got %q", j.Kind)
	}
	b, err := json.Marshal(pendingJob{Kind: j.Kind, ProjectID: j.ProjectID, ProjectPath: j.ProjectPath, MRIID: j.MRIID,
		HeadSHA: j.HeadSHA, ForceFull: j.ForceFull, EnqueuedAt: j.EnqueuedAt.UTC()})
	if err != nil {
		return fmt.Errorf("store: save pending job: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO queue_pending(project_id, mr_iid, job, ready_at, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, mr_iid) DO UPDATE SET
		   job = excluded.job, ready_at = excluded.ready_at, updated_at = excluded.updated_at`,
		j.ProjectID, j.MRIID, string(b), toNanos(p.ReadyAt), toNanos(time.Now()))
	if err != nil {
		return fmt.Errorf("store: save pending job: %w", err)
	}
	return nil
}

// DeletePending forgets the pending automatic review of an MR.
func (s *SQLite) DeletePending(ctx context.Context, projectID, mrIID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM queue_pending WHERE project_id = ? AND mr_iid = ?`, projectID, mrIID); err != nil {
		return fmt.Errorf("store: delete pending job: %w", err)
	}
	return nil
}

// ListPending returns every stored pending automatic review, oldest ready
// first. Unreadable rows are skipped.
func (s *SQLite) ListPending(ctx context.Context) ([]queue.Pending, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job, ready_at FROM queue_pending ORDER BY ready_at, project_id, mr_iid`)
	if err != nil {
		return nil, fmt.Errorf("store: list pending jobs: %w", err)
	}
	defer rows.Close()
	var out []queue.Pending
	for rows.Next() {
		var (
			raw string
			at  int64
		)
		if err := rows.Scan(&raw, &at); err != nil {
			return nil, fmt.Errorf("store: list pending jobs: %w", err)
		}
		var pj pendingJob
		if err := json.Unmarshal([]byte(raw), &pj); err != nil || pj.Kind != core.JobAutoReview || pj.ProjectID == 0 || pj.MRIID == 0 {
			continue
		}
		out = append(out, queue.Pending{Job: core.Job{Kind: pj.Kind, ProjectID: pj.ProjectID, ProjectPath: pj.ProjectPath,
			MRIID: pj.MRIID, HeadSHA: pj.HeadSHA, ForceFull: pj.ForceFull, EnqueuedAt: pj.EnqueuedAt}, ReadyAt: fromNanos(at)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list pending jobs: %w", err)
	}
	return out, nil
}
