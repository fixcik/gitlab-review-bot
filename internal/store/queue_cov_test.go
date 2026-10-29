package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
)

// outOfRange is a time encoding/json refuses (year > 9999).
var outOfRange = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)

// A job that cannot be encoded is refused with an error and nothing is
// stored (no half-written row a restart would drop as invalid).
func TestCovQueueEncodeErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if id, err := s.AddJob(ctx, queue.Stored{Job: core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 2, EnqueuedAt: outOfRange}}); err == nil || id != 0 ||
		!strings.Contains(err.Error(), "store: add queued job") {
		t.Fatalf("AddJob with an unencodable time: %d %v", id, err)
	}
	pipe := &core.PipelineRef{ID: 1, At: outOfRange}
	if _, err := s.AddJob(ctx, queue.Stored{Job: core.Job{Kind: core.JobPipeline, ProjectID: 1, MRIID: 2, Pipeline: pipe}}); err == nil {
		t.Fatal("AddJob with an unencodable pipeline time accepted")
	}
	if err := s.SavePending(ctx, queue.Pending{Job: core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 2, EnqueuedAt: outOfRange}}); err == nil ||
		!strings.Contains(err.Error(), "store: save pending job") {
		t.Fatalf("SavePending with an unencodable time: %v", err)
	}
	if err := s.SavePending(ctx, queue.Pending{Job: core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 2}}); err == nil {
		t.Fatal("SavePending accepted a job that is not an automatic review")
	}
	if l, err := s.ListJobs(ctx); err != nil || len(l) != 0 {
		t.Fatalf("jobs stored: %+v %v", l, err)
	}
	if l, err := s.ListPending(ctx); err != nil || len(l) != 0 {
		t.Fatalf("pending stored: %+v %v", l, err)
	}
}

// Every queue persistence call reports a database failure as an error
// (the queue logs it and keeps the job in memory).
func TestCovQueueDBErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	s.Close()
	auto := core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 2}
	chat := core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 2, NoteID: 3}
	// key: the error's prefix after "store: ".
	checks := map[string]error{
		"save pending job":   s.SavePending(ctx, queue.Pending{Job: auto}),
		"delete pending job": s.DeletePending(ctx, 1, 2),
		"update queued job":  s.UpdateJob(ctx, queue.Stored{ID: 1, Job: chat}),
		"delete queued job":  s.FinishJob(ctx, 1, time.Now()),
	}
	_, checks["list finished jobs"] = s.ListDone(ctx, time.Time{})
	_, checks["add queued job"] = s.AddJob(ctx, queue.Stored{Job: chat})
	_, checks["list queued jobs"] = s.ListJobs(ctx)
	_, checks["list pending jobs"] = s.ListPending(ctx)
	for name, err := range checks {
		if err == nil || !strings.Contains(err.Error(), "store: "+name) {
			t.Errorf("%s on a closed database: %v", name, err)
		}
	}
}

// A row the driver cannot read (a non-integer time) fails the whole list
// instead of restoring a job with a wrong ready time.
func TestCovQueueScanErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO queue_jobs(project_id, mr_iid, kind, job, enqueued_at, ready_at, updated_at)
		VALUES (1, 2, 'chat', '{"kind":"chat","project_id":1,"mr_iid":2}', 1, 'soon', 1)`); err != nil {
		t.Fatal(err)
	}
	if l, err := s.ListJobs(ctx); err == nil || !strings.Contains(err.Error(), "store: list queued jobs") {
		t.Fatalf("ListJobs over an unreadable row: %+v %v", l, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO queue_pending(project_id, mr_iid, job, ready_at, updated_at)
		VALUES (1, 2, '{"kind":"auto_review","project_id":1,"mr_iid":2}', 'soon', 1)`); err != nil {
		t.Fatal(err)
	}
	if l, err := s.ListPending(ctx); err == nil || !strings.Contains(err.Error(), "store: list pending jobs") {
		t.Fatalf("ListPending over an unreadable row: %+v %v", l, err)
	}
}

// An error while stepping through the rows (after the query started) fails
// the list instead of returning the rows read so far as the whole queue.
// The tables are replaced by views whose second row fails at run time
// (abs of the smallest integer overflows).
func TestCovQueueIterationErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	for _, q := range []string{
		`DROP TABLE queue_jobs`,
		`CREATE VIEW queue_jobs AS
		   SELECT 1 AS id, 1 AS project_id, 2 AS mr_iid, '{"kind":"chat","project_id":1,"mr_iid":2}' AS job,
		          1 AS ready_at, 0 AS started_at, 0 AS answered, 0 AS charged
		   UNION ALL
		   SELECT 2, 1, 3, '{"kind":"chat","project_id":1,"mr_iid":3}', abs(-9223372036854775807 - 1), 0, 0, 0`,
		`DROP TABLE queue_pending`,
		`CREATE VIEW queue_pending AS
		   SELECT 1 AS project_id, 2 AS mr_iid, '{"kind":"auto_review","project_id":1,"mr_iid":2}' AS job, 1 AS ready_at
		   UNION ALL
		   SELECT 1, 3, '{"kind":"auto_review","project_id":1,"mr_iid":3}', abs(-9223372036854775807 - 1)`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if l, err := s.ListJobs(ctx); err == nil || !strings.Contains(err.Error(), "store: list queued jobs") {
		t.Fatalf("ListJobs with a failing row: %+v %v", l, err)
	}
	if l, err := s.ListPending(ctx); err == nil || !strings.Contains(err.Error(), "store: list pending jobs") {
		t.Fatalf("ListPending with a failing row: %+v %v", l, err)
	}
}

// A row whose job decodes but has no kind is unusable too: it comes back
// with the row's own project and MR (not the ones in its JSON) and no
// kind, so the queue drops it.
func TestCovQueueJobsRowWithoutKind(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO queue_jobs(project_id, mr_iid, kind, job, enqueued_at, ready_at, updated_at)
		VALUES (1, 2, 'chat', '{"project_id":5,"mr_iid":6,"note_id":7}', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	l, err := s.ListJobs(ctx)
	if err != nil || len(l) != 1 {
		t.Fatalf("%+v %v", l, err)
	}
	if j := l[0].Job; j.Kind != "" || j.ProjectID != 1 || j.MRIID != 2 || j.NoteID != 0 {
		t.Fatalf("row without kind: %+v", j)
	}
}

// A refused or failed AddJob returns id 0 (never an id the queue could
// later update or delete).
func TestCovAddJobErrorReturnsZeroID(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if id, err := s.AddJob(ctx, queue.Stored{Job: core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 2}}); err == nil || id != 0 {
		t.Fatalf("auto review: %d %v", id, err)
	}
	s.Close()
	if id, err := s.AddJob(ctx, queue.Stored{Job: core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 2}}); err == nil || id != 0 {
		t.Fatalf("closed database: %d %v", id, err)
	}
}
