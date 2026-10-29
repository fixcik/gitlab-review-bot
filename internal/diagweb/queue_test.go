package diagweb

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

type fakeQueue struct {
	jobs             []queue.Stored
	pending          []queue.Pending
	jobsErr, pendErr error
}

func (q fakeQueue) ListJobs(context.Context) ([]queue.Stored, error)     { return q.jobs, q.jobsErr }
func (q fakeQueue) ListPending(context.Context) ([]queue.Pending, error) { return q.pending, q.pendErr }

// The queue block is absent without a source or when it cannot be read
// (the list still answers), present with zero counts when the queue is
// empty (the page then says so).
func TestQueueBlockAbsentOrEmpty(t *testing.T) {
	if d := listData(t, newFixture(t, Options{Enabled: true}).h, ""); d.Queue != nil || len(d.Runs) != 3 {
		t.Fatalf("no source: %+v", d.Queue)
	}
	for name, q := range map[string]fakeQueue{"jobs": {jobsErr: errors.New("boom")}, "pending": {pendErr: errors.New("boom")}} {
		if d := listData(t, newFixture(t, Options{Enabled: true, Queue: q}).h, ""); d.Queue != nil || len(d.Runs) != 3 {
			t.Errorf("%s error: %+v", name, d.Queue)
		}
	}
	rec := get(newFixture(t, Options{Enabled: true, Queue: fakeQueue{}}).h, DataPath, nil)
	if !strings.Contains(rec.Body.String(), `"queue":{"running":0,"waiting":0,"items":[]}`) {
		t.Fatalf("empty queue: %s", rec.Body)
	}
}

func TestQueueBlockItems(t *testing.T) {
	t0 := now.Add(-time.Hour)
	q := fakeQueue{
		jobs: []queue.Stored{
			{ID: 1, Job: core.Job{Kind: core.JobChat, ProjectPath: "g/api", MRIID: 7, Username: "alice", Question: "SECRET QUESTION",
				DiscussionID: "d1", NoteID: 5, EnqueuedAt: t0}, ReadyAt: t0, StartedAt: t0.Add(2 * time.Minute)},
			{ID: 2, Job: core.Job{Kind: core.JobReview, ProjectPath: "g/web", MRIID: 3, Username: "bob", EnqueuedAt: t0}, ReadyAt: now.Add(time.Minute)},
			{ID: 3, Job: core.Job{Kind: core.JobFullReview, ProjectPath: "g/web", MRIID: 4, EnqueuedAt: t0}, ReadyAt: t0, StartedAt: t0.Add(time.Minute)},
			{ID: 4, Job: core.Job{ProjectID: 9, MRIID: 1}}, // unreadable row
		},
		pending: []queue.Pending{{Job: core.Job{Kind: core.JobAutoReview, ProjectPath: "g/api", MRIID: 8, EnqueuedAt: t0}, ReadyAt: now.Add(-time.Minute)}},
	}
	f := newFixture(t, Options{Enabled: true, Queue: q})
	d := listData(t, f.h, "?status=failed") // the queue ignores the list filters
	if len(d.Runs) != 1 || d.Queue == nil || d.Queue.Running != 2 || d.Queue.Waiting != 2 || len(d.Queue.Items) != 4 {
		t.Fatalf("queue: %+v", d.Queue)
	}
	it := d.Queue.Items
	// Running by start (longest first), then waiting by due time.
	if it[0].Kind != "full_review" || it[0].State != "running" || it[1].Kind != "chat" || it[1].Username != "alice" || !it[1].StartedAt.Equal(t0.Add(2*time.Minute)) ||
		it[2].Kind != "auto_review" || !it[2].Auto || it[2].State != "waiting" || it[2].MRIID != 8 ||
		it[3].Kind != "review" || it[3].State != "waiting" || !it[3].ReadyAt.Equal(now.Add(time.Minute)) || it[3].Auto {
		t.Fatalf("items: %+v", it)
	}
	body := get(f.h, DataPath, nil).Body.String()
	for _, s := range []string{"SECRET QUESTION", `"d1"`, "note_id", "discussion"} {
		if strings.Contains(body, s) {
			t.Errorf("queue block leaks %q", s)
		}
	}
	// The counts stay exact, the items are capped.
	var many fakeQueue
	for i := 0; i < maxQueueItems+5; i++ {
		many.jobs = append(many.jobs, queue.Stored{ID: int64(i), Job: core.Job{Kind: core.JobChat, ProjectPath: "g/api", MRIID: int64(i + 1)}, ReadyAt: t0})
	}
	if d := listData(t, newFixture(t, Options{Enabled: true, Queue: many}).h, ""); d.Queue.Waiting != maxQueueItems+5 || len(d.Queue.Items) != maxQueueItems {
		t.Fatalf("cap: %d %d", d.Queue.Waiting, len(d.Queue.Items))
	}
}

// The store is a QueueSource as it is (the wiring in cmd/reviewbot).
func TestQueueBlockFromStore(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.AddJob(ctx, queue.Stored{Job: core.Job{Kind: core.JobChat, ProjectID: 1, ProjectPath: "g/api", MRIID: 2, EnqueuedAt: now}, ReadyAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePending(ctx, queue.Pending{Job: core.Job{Kind: core.JobAutoReview, ProjectID: 1, ProjectPath: "g/api", MRIID: 3, EnqueuedAt: now}, ReadyAt: now}); err != nil {
		t.Fatal(err)
	}
	d := listData(t, newFixture(t, Options{Enabled: true, Queue: st}).h, "")
	if d.Queue == nil || d.Queue.Waiting != 2 || d.Queue.Running != 0 {
		t.Fatalf("store queue: %+v", d.Queue)
	}
}
