package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
)

// notStored are the core.Job fields the job table deliberately leaves out.
var notStored = map[string]bool{
	"TraceParent": true, // a restored job has no link to the event's trace (documented on core.Job)
}

// fullJob sets every core.Job field to a non-zero value (reflection, so a
// new field fails the round trip below until storedJob covers it).
func fullJob(t *testing.T) core.Job {
	t.Helper()
	at := time.Date(2026, 10, 27, 9, 30, 0, 123, time.UTC)
	j := core.Job{Kind: core.JobChat, ProjectID: 42, ProjectPath: "g/api", MRIID: 7, HeadSHA: "abc",
		UserID: 9, Username: "dev", DiscussionID: "d1", NoteID: 1001, Question: "why?", ForceFull: true,
		Pipeline: &core.PipelineRef{ID: 5, Ref: "main", SHA: "def", Status: "failed", Main: true, DefaultBranch: "main",
			Source: "push", At: at.Add(-time.Minute)},
		EnqueuedAt: at, TraceParent: "00-x-y-01"}
	v := reflect.ValueOf(j)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Fatalf("fullJob leaves core.Job.%s zero: set it (and store it, or list it in notStored)", v.Type().Field(i).Name)
		}
	}
	return j
}

// Every core.Job field a non-auto job needs survives a restart (only
// notStored fields are dropped), with the queue state, in enqueue order.
func TestQueueJobsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	j := fullJob(t)
	ready := j.EnqueuedAt.Add(time.Hour)
	id1, err := s.AddJob(ctx, queue.Stored{Job: j, ReadyAt: ready})
	if err != nil {
		t.Fatal(err)
	}
	second := core.Job{Kind: core.JobHelp, ProjectID: 42, MRIID: 7, NoteID: 1002, EnqueuedAt: j.EnqueuedAt}
	id2, err := s.AddJob(ctx, queue.Stored{Job: second, ReadyAt: j.EnqueuedAt})
	if err != nil {
		t.Fatal(err)
	}
	if id2 <= id1 {
		t.Fatalf("ids %d, %d not increasing", id1, id2)
	}
	if _, err := s.AddJob(ctx, queue.Stored{Job: core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 1}}); err == nil {
		t.Fatal("auto review accepted by AddJob")
	}
	started := j.EnqueuedAt.Add(2 * time.Minute)
	if err := s.UpdateJob(ctx, queue.Stored{ID: id1, Job: j, ReadyAt: ready, StartedAt: started, Answered: true, Charged: true}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := OpenSQLite(path) // restart
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	list, err := s2.ListJobs(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %+v %v", list, err)
	}
	got := list[0]
	if got.ID != id1 || !got.ReadyAt.Equal(ready) || !got.StartedAt.Equal(started) || !got.Answered || !got.Charged {
		t.Fatalf("state %+v", got)
	}
	want := j
	gv, wv := reflect.ValueOf(got.Job), reflect.ValueOf(want)
	for i := 0; i < wv.NumField(); i++ {
		name := wv.Type().Field(i).Name
		if notStored[name] {
			if !gv.Field(i).IsZero() {
				t.Errorf("%s stored, but listed as not stored", name)
			}
			continue
		}
		if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
			t.Errorf("core.Job.%s: got %v, want %v (store it in storedJob or list it in notStored)", name, gv.Field(i), wv.Field(i))
		}
	}
	if list[1].ID != id2 || list[1].Job.Kind != core.JobHelp || !list[1].StartedAt.IsZero() || list[1].Answered || list[1].Job.Pipeline != nil {
		t.Fatalf("second %+v", list[1])
	}
	if err := s2.FinishJob(ctx, id1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if list, _ := s2.ListJobs(ctx); len(list) != 1 || list[0].ID != id2 {
		t.Fatalf("after delete %+v", list)
	}
	// AUTOINCREMENT: an id is never reused, so a later job sorts last.
	if err := s2.FinishJob(ctx, id2, time.Now()); err != nil {
		t.Fatal(err)
	}
	if id3, err := s2.AddJob(ctx, queue.Stored{Job: second}); err != nil || id3 <= id2 {
		t.Fatalf("id after deleting the newest: %d %v", id3, err)
	}
}

// An unreadable row comes back with its ids and no kind (the queue drops
// it); the rows after it are intact.
func TestQueueJobsBadRow(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO queue_jobs(project_id, mr_iid, kind, job, enqueued_at, ready_at, updated_at)
		VALUES (1, 2, 'chat', '{not json', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddJob(ctx, queue.Stored{Job: core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 3, NoteID: 4}}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListJobs(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %+v %v", list, err)
	}
	if list[0].Job.Kind != "" || list[0].Job.MRIID != 2 || list[1].Job.NoteID != 4 {
		t.Fatalf("list %+v", list)
	}
}

// A finished job leaves queue_jobs and is listed by ListDone (by finish
// time) for queue.DoneTTL; older markers are forgotten by the next
// FinishJob, unreadable ones are skipped. Finishing a row that is gone
// keeps no marker.
func TestQueueJobsFinishKeepsMarker(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	t0 := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	add := func(note int64) int64 {
		id, err := s.AddJob(ctx, queue.Stored{Job: core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 2, NoteID: note}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	notes := func(since time.Time) []int64 {
		l, err := s.ListDone(ctx, since)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, j := range l {
			out = append(out, j.NoteID)
		}
		return out
	}
	id1, id2 := add(1), add(2)
	if err := s.FinishJob(ctx, id1, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO queue_done(job, done_at) VALUES ('{not json', ?)`, toNanos(t0)); err != nil {
		t.Fatal(err)
	}
	if got := notes(t0); len(got) != 1 || got[0] != 1 {
		t.Fatalf("done %v, want [1]", got)
	}
	if got := notes(t0.Add(time.Second)); len(got) != 0 {
		t.Fatalf("done since later %v", got)
	}
	if err := s.FinishJob(ctx, id2, t0.Add(queue.DoneTTL+time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, id1, t0.Add(queue.DoneTTL+time.Second)); err != nil { // gone already
		t.Fatal(err)
	}
	if got := notes(time.Time{}); len(got) != 1 || got[0] != 2 {
		t.Fatalf("done %v, want [2] (note 1 and the unreadable marker pruned)", got)
	}
	if l, err := s.ListJobs(ctx); err != nil || len(l) != 0 {
		t.Fatalf("rows left %+v %v", l, err)
	}
}
