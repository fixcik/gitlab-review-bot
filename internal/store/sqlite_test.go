package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
)

func openTest(t *testing.T) (*SQLite, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "dir", "bot.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestMRStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)

	got, err := s.GetMRState(ctx, 1, 2)
	if err != nil || got != nil {
		t.Fatalf("absent state: got %v, %v; want nil, nil", got, err)
	}

	ts := time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("X", 3*3600))
	want := core.MRState{ProjectID: 1, MRIID: 2, ReviewedSHA: "abc", SummaryNoteID: 10, UpdatedAt: ts}
	if err := s.SaveMRState(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetMRState(ctx, 1, 2)
	if err != nil || got == nil {
		t.Fatalf("GetMRState: %v, %v", got, err)
	}
	if got.ReviewedSHA != "abc" || got.SummaryNoteID != 10 || !got.UpdatedAt.Equal(ts) || got.UpdatedAt.Location() != time.UTC {
		t.Fatalf("unexpected state %+v", got)
	}

	// Upsert.
	want.ReviewedSHA, want.SummaryNoteID = "def", 11
	if err := s.SaveMRState(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMRState(ctx, 1, 2)
	if got.ReviewedSHA != "def" || got.SummaryNoteID != 11 {
		t.Fatalf("upsert failed: %+v", got)
	}
	if other, _ := s.GetMRState(ctx, 1, 3); other != nil {
		t.Fatalf("unexpected state for other MR: %+v", other)
	}
}

func TestUsageFiltersAndOrder(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	recs := []core.Usage{
		{UserID: 1, Kind: core.UsageRerun, MRKey: "1!1", At: base.Add(3 * time.Minute)},
		{UserID: 2, Kind: core.UsageRerun, MRKey: "1!1", At: base.Add(1 * time.Minute)},
		{UserID: 1, Kind: core.UsageChat, MRKey: "1!2", At: base.Add(2 * time.Minute)},
		{UserID: 0, Kind: core.UsageAuto, MRKey: "1!1", At: base},
	}
	for _, r := range recs {
		if err := s.RecordUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ListUsage(ctx, core.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("len=%d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].At.Before(all[i-1].At) {
			t.Fatalf("not oldest first: %+v", all)
		}
	}
	if all[0].Kind != core.UsageAuto || all[0].MRKey != "1!1" || all[0].UserID != 0 {
		t.Fatalf("unexpected first: %+v", all[0])
	}

	uid := int64(1)
	rerun := core.UsageRerun
	mr := "1!1"
	cases := []struct {
		name string
		f    core.UsageFilter
		want int
	}{
		{"user", core.UsageFilter{UserID: &uid}, 2},
		{"kind", core.UsageFilter{Kind: &rerun}, 2},
		{"mr", core.UsageFilter{MRKey: &mr}, 3},
		{"user+kind", core.UsageFilter{UserID: &uid, Kind: &rerun}, 1},
		{"mr+kind", core.UsageFilter{MRKey: &mr, Kind: &rerun}, 2},
		{"since inclusive", core.UsageFilter{Since: base.Add(2 * time.Minute)}, 2},
		{"since+kind", core.UsageFilter{Since: base.Add(2 * time.Minute), Kind: &rerun}, 1},
	}
	for _, c := range cases {
		got, err := s.ListUsage(ctx, c.f)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != c.want {
			t.Errorf("%s: got %d records, want %d: %+v", c.name, len(got), c.want, got)
		}
	}

	if err := s.PruneUsage(ctx, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ListUsage(ctx, core.UsageFilter{})
	if len(all) != 2 || !all[0].At.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("after prune: %+v", all)
	}
}

func TestReopenKeepsDataAndVersion(t *testing.T) {
	ctx := context.Background()
	s, path := openTest(t)
	if err := s.SaveMRState(ctx, core.MRState{ProjectID: 5, MRIID: 6, ReviewedSHA: "x"}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetMRState(ctx, 5, 6)
	if err != nil || got == nil || got.ReviewedSHA != "x" || got.UpdatedAt.IsZero() {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
	var v int
	if err := s2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("version=%d err=%v", v, err)
	}
	var mode string
	if err := s2.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode=%q err=%v", mode, err)
	}
}

func TestQueuePendingPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.Date(2026, 1, 2, 15, 40, 0, 0, time.UTC)
	j := core.Job{Kind: core.JobAutoReview, ProjectID: 42, ProjectPath: "g/api", MRIID: 7, HeadSHA: "abc", ForceFull: true,
		EnqueuedAt: at.Add(-time.Hour), UserID: 9, Question: "never stored"}
	if err := s.SavePending(ctx, queue.Pending{Job: j, ReadyAt: at}); err != nil {
		t.Fatal(err)
	}
	// Same MR again: replaced (one row per MR).
	j.HeadSHA = "def"
	if err := s.SavePending(ctx, queue.Pending{Job: j, ReadyAt: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePending(ctx, queue.Pending{Job: core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 1}}); err == nil {
		t.Fatal("manual jobs must not be persisted")
	}
	if err := s.SavePending(ctx, queue.Pending{Job: core.Job{Kind: core.JobAutoReview, ProjectID: 42, MRIID: 8}, ReadyAt: at}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := OpenSQLite(path) // restart
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	list, err := s2.ListPending(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %+v %v", list, err)
	}
	got := list[1] // ordered by ready_at: MR 8 first
	if got.Job.MRIID != 7 || got.Job.HeadSHA != "def" || !got.Job.ForceFull || got.Job.ProjectPath != "g/api" ||
		!got.ReadyAt.Equal(at.Add(time.Minute)) || got.Job.UserID != 0 || got.Job.Question != "" || !got.Job.EnqueuedAt.Equal(at.Add(-time.Hour)) {
		t.Fatalf("restored %+v", got)
	}
	if err := s2.DeletePending(ctx, 42, 7); err != nil {
		t.Fatal(err)
	}
	if list, _ := s2.ListPending(ctx); len(list) != 1 || list[0].Job.MRIID != 8 {
		t.Fatalf("after delete %+v", list)
	}
}
