package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
)

// Queued jobs that are not automatic reviews (queue.Persister AddJob,
// UpdateJob, FinishJob, ListJobs, ListDone): one row per job, so a restart
// does not lose commands, questions, thread replies, refreshes or pipeline
// jobs, queued or running. The id only grows (AUTOINCREMENT), so ordering
// by it restores the enqueue order. started_at / answered / charged drive
// the replay rule after an interrupted run (see queue.Stored). A finished
// job moves to queue_done for queue.DoneTTL: the event dedup of the next
// start (a separate table, so a binary without it never replays them).
func init() {
	addNamedMigration("queue_jobs_v1", `
	CREATE TABLE IF NOT EXISTS queue_jobs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id  INTEGER NOT NULL,
		mr_iid      INTEGER NOT NULL,
		kind        TEXT    NOT NULL,
		job         TEXT    NOT NULL,
		enqueued_at INTEGER NOT NULL,
		ready_at    INTEGER NOT NULL,
		started_at  INTEGER NOT NULL DEFAULT 0,
		answered    INTEGER NOT NULL DEFAULT 0,
		charged     INTEGER NOT NULL DEFAULT 0,
		updated_at  INTEGER NOT NULL
	);`)
	addNamedMigration("queue_done_v1", `
	CREATE TABLE IF NOT EXISTS queue_done (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		job     TEXT    NOT NULL,
		done_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS queue_done_at ON queue_done(done_at);`)
}

// storedJob is the stored form of a core.Job that is not an automatic
// review: explicit fields (a rename in core does not silently change the
// stored format). TraceParent is deliberately not stored: a restored job
// starts a trace without a link to the event (see core.Job).
type storedJob struct {
	Kind         core.JobKind    `json:"kind"`
	ProjectID    int64           `json:"project_id"`
	ProjectPath  string          `json:"project_path"`
	MRIID        int64           `json:"mr_iid"`
	HeadSHA      string          `json:"head_sha,omitempty"`
	UserID       int64           `json:"user_id,omitempty"`
	Username     string          `json:"username,omitempty"`
	DiscussionID string          `json:"discussion_id,omitempty"`
	NoteID       int64           `json:"note_id,omitempty"`
	Question     string          `json:"question,omitempty"`
	ForceFull    bool            `json:"force_full,omitempty"`
	Pipeline     *storedPipeline `json:"pipeline,omitempty"`
	EnqueuedAt   time.Time       `json:"enqueued_at"`
}

type storedPipeline struct {
	ID            int64     `json:"id"`
	Ref           string    `json:"ref,omitempty"`
	SHA           string    `json:"sha,omitempty"`
	Status        string    `json:"status,omitempty"`
	Main          bool      `json:"main,omitempty"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	Source        string    `json:"source,omitempty"`
	At            time.Time `json:"at,omitempty"`
}

func toStoredJob(j core.Job) storedJob {
	sj := storedJob{Kind: j.Kind, ProjectID: j.ProjectID, ProjectPath: j.ProjectPath, MRIID: j.MRIID, HeadSHA: j.HeadSHA,
		UserID: j.UserID, Username: j.Username, DiscussionID: j.DiscussionID, NoteID: j.NoteID, Question: j.Question,
		ForceFull: j.ForceFull, EnqueuedAt: j.EnqueuedAt.UTC()}
	if p := j.Pipeline; p != nil {
		sj.Pipeline = &storedPipeline{ID: p.ID, Ref: p.Ref, SHA: p.SHA, Status: p.Status, Main: p.Main,
			DefaultBranch: p.DefaultBranch, Source: p.Source, At: p.At.UTC()}
	}
	return sj
}

func (sj storedJob) job() core.Job {
	j := core.Job{Kind: sj.Kind, ProjectID: sj.ProjectID, ProjectPath: sj.ProjectPath, MRIID: sj.MRIID, HeadSHA: sj.HeadSHA,
		UserID: sj.UserID, Username: sj.Username, DiscussionID: sj.DiscussionID, NoteID: sj.NoteID, Question: sj.Question,
		ForceFull: sj.ForceFull, EnqueuedAt: sj.EnqueuedAt}
	if p := sj.Pipeline; p != nil {
		j.Pipeline = &core.PipelineRef{ID: p.ID, Ref: p.Ref, SHA: p.SHA, Status: p.Status, Main: p.Main,
			DefaultBranch: p.DefaultBranch, Source: p.Source, At: p.At}
	}
	return j
}

// AddJob stores a new queued job and returns its id.
func (s *SQLite) AddJob(ctx context.Context, st queue.Stored) (int64, error) {
	j := st.Job
	if j.Kind == core.JobAutoReview {
		return 0, fmt.Errorf("store: automatic reviews are stored with SavePending")
	}
	b, err := json.Marshal(toStoredJob(j))
	if err != nil {
		return 0, fmt.Errorf("store: add queued job: %w", err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`INSERT INTO queue_jobs(project_id, mr_iid, kind, job, enqueued_at, ready_at, started_at, answered, charged, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		j.ProjectID, j.MRIID, string(j.Kind), string(b), toNanos(j.EnqueuedAt), toNanos(st.ReadyAt), toNanos(st.StartedAt),
		boolInt(st.Answered), boolInt(st.Charged), toNanos(time.Now())).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: add queued job: %w", err)
	}
	return id, nil
}

// UpdateJob writes the queue state of a stored job (ready time, start,
// answered, charged). The job itself does not change.
func (s *SQLite) UpdateJob(ctx context.Context, st queue.Stored) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE queue_jobs SET ready_at = ?, started_at = ?, answered = ?, charged = ?, updated_at = ? WHERE id = ?`,
		toNanos(st.ReadyAt), toNanos(st.StartedAt), boolInt(st.Answered), boolInt(st.Charged), toNanos(time.Now()), st.ID); err != nil {
		return fmt.Errorf("store: update queued job: %w", err)
	}
	return nil
}

// FinishJob moves a finished queued job to queue_done (finished at at) and
// forgets the ones finished before at - queue.DoneTTL. One call, in this
// order: a crash in between leaves the row and its marker (the queue's
// replay rule decides about the row; the marker only feeds the dedup).
func (s *SQLite) FinishJob(ctx context.Context, id int64, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO queue_done(job, done_at) SELECT job, ? FROM queue_jobs WHERE id = ?;
		 DELETE FROM queue_jobs WHERE id = ?;
		 DELETE FROM queue_done WHERE done_at < ?;`,
		toNanos(at), id, id, toNanos(at.Add(-queue.DoneTTL))); err != nil {
		return fmt.Errorf("store: delete queued job: %w", err)
	}
	return nil
}

// ListDone returns the jobs finished at or after since (FinishJob), oldest
// first; unreadable ones are skipped.
func (s *SQLite) ListDone(ctx context.Context, since time.Time) ([]core.Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job FROM queue_done WHERE done_at >= ? ORDER BY id`, toNanos(since))
	if err != nil {
		return nil, fmt.Errorf("store: list finished jobs: %w", err)
	}
	defer rows.Close()
	var out []core.Job
	for rows.Next() {
		var (
			raw string
			sj  storedJob
		)
		if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &sj) == nil && sj.Kind != "" {
			out = append(out, sj.job())
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list finished jobs: %w", err)
	}
	return out, nil
}

// ListJobs returns every stored queued job by id (enqueue order). A row
// whose job cannot be read comes back with only its id, project and MR
// (zero Kind), so the queue drops it instead of keeping it forever.
func (s *SQLite) ListJobs(ctx context.Context) ([]queue.Stored, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, mr_iid, job, ready_at, started_at, answered, charged FROM queue_jobs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list queued jobs: %w", err)
	}
	defer rows.Close()
	var out []queue.Stored
	for rows.Next() {
		var (
			id, pid, iid      int64
			raw               string
			ready, started    int64
			answered, charged int
		)
		if err := rows.Scan(&id, &pid, &iid, &raw, &ready, &started, &answered, &charged); err != nil {
			return nil, fmt.Errorf("store: list queued jobs: %w", err)
		}
		st := queue.Stored{ID: id, ReadyAt: fromNanos(ready), StartedAt: fromNanos(started), Answered: answered != 0, Charged: charged != 0}
		var sj storedJob
		if err := json.Unmarshal([]byte(raw), &sj); err != nil || sj.Kind == "" {
			st.Job = core.Job{ProjectID: pid, MRIID: iid}
		} else {
			st.Job = sj.job()
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list queued jobs: %w", err)
	}
	return out, nil
}
