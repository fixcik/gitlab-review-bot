package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

// Only a single ALTER TABLE ... ADD COLUMN statement may treat "duplicate
// column name" as "already applied"; anything else must fail loudly, or the
// remaining statements of the step would be skipped yet recorded as done.
func TestMutAddsColumn(t *testing.T) {
	cases := map[string]bool{
		"ALTER TABLE t ADD COLUMN c TEXT NOT NULL DEFAULT '';":         true,
		"\n\talter table t add column c INTEGER;\n":                    true,
		"ALTER TABLE t ADD COLUMN c TEXT; CREATE INDEX i ON t(c);":     false,
		"ALTER TABLE t RENAME COLUMN a TO b;":                          false,
		"CREATE TABLE x (a INT); ALTER TABLE x ADD COLUMN b INT;":      false,
		"CREATE INDEX IF NOT EXISTS i ON t(c) -- ADD COLUMN mentioned": false,
	}
	for q, want := range cases {
		if got := addsColumn(q); got != want {
			t.Errorf("addsColumn(%q) = %v, want %v", q, got, want)
		}
	}
}

// A database that already has a column added by a named step, but no record
// of the step (e.g. a branch build applied it under another name), opens
// fine and the step is recorded; a multi-statement step hitting a duplicate
// column is an error.
func TestMutAddColumnStepIdempotent(t *testing.T) {
	ctx := context.Background()
	s, path := openTest(t)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = 'sysmap_runs_kind_v1'`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen with the column already present: %v", err)
	}
	var n int
	if err := s2.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name = 'sysmap_runs_kind_v1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("step not recorded: n=%d err=%v", n, err)
	}
	s2.Close()

	saved := namedMigrations
	t.Cleanup(func() { namedMigrations = saved })
	namedMigrations = append(append(namedMigrations[:0:0], saved...), struct {
		name string
		sql  string
	}{"mut_multi_v1", `ALTER TABLE sysmap_runs ADD COLUMN kind TEXT NOT NULL DEFAULT ''; CREATE INDEX IF NOT EXISTS mut_i ON sysmap_runs(kind);`})
	if s3, err := OpenSQLite(path); err == nil {
		s3.Close()
		t.Fatal("multi-statement step with a duplicate column must fail")
	}
}

// Lookups by hash work for a single hash and across the 400-hash chunk
// boundary; deletes by path likewise.
func TestMutSysmapChunking(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var files []sysmap.FileRecord
	var hashes, paths []string
	for i := 0; i < 401; i++ {
		h, p := fmt.Sprintf("h%03d", i), fmt.Sprintf("f%03d.go", i)
		files = append(files, sysmap.FileRecord{Path: p, Hash: h, Version: sysmap.CodeVersion, ExtractedAt: now})
		hashes, paths = append(hashes, h), append(paths, p)
	}
	if err := s.SaveSysmapFiles(ctx, "g/r", files); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SysmapEntriesByHash(ctx, sysmap.CodeVersion, []string{"h007"}); err != nil || len(got) != 1 {
		t.Fatalf("single hash: %v %v", got, err)
	}
	got, err := s.SysmapEntriesByHash(ctx, sysmap.CodeVersion, hashes)
	if err != nil || len(got) != 401 {
		t.Fatalf("401 hashes: got %d, err %v", len(got), err)
	}
	if err := s.DeleteSysmapFiles(ctx, "g/r", paths); err != nil {
		t.Fatal(err)
	}
	if left, err := s.ListSysmapFiles(ctx, "g/r"); err != nil || len(left) != 0 {
		t.Fatalf("after delete: %d files left, err %v", len(left), err)
	}
}

// Sysmap run counts are per kind and include a run exactly at since; old
// runs are pruned relative to the newest record's time.
func TestMutSysmapRunCountAndPrune(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)
	for _, r := range []sysmap.RunRecord{
		{At: since, Kind: sysmap.RunKindScan},
		{At: since.Add(-time.Nanosecond), Kind: sysmap.RunKindScan},
		{At: now.Add(-sysmapRunsKeep - time.Hour), Kind: sysmap.RunKindDeps},
		{At: now, Kind: sysmap.RunKindDeps},
	} {
		if err := s.RecordSysmapRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.SysmapRunCount(ctx, since, sysmap.RunKindScan); err != nil || n != 1 {
		t.Fatalf("scan count %d %v", n, err)
	}
	if n, err := s.SysmapRunCount(ctx, time.Time{}, sysmap.RunKindDeps); err != nil || n != 1 {
		t.Fatalf("deps count (very old run must be pruned) %d %v", n, err)
	}
}

// Unreadable or invalid pending rows (corrupt JSON, not an automatic review,
// zero ids) are skipped one by one: the valid rows after them still come
// back after a restart.
func TestMutListPendingSkipsBadRowsOnly(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	bad := []struct {
		pid, iid int64
		job      string
	}{
		{1, 1, `{not json`},
		{1, 2, `{"kind":"chat","project_id":1,"mr_iid":2}`},
		{1, 3, `{"kind":"auto_review","project_id":0,"mr_iid":3}`},
		{1, 4, `{"kind":"auto_review","project_id":1,"mr_iid":0}`},
	}
	for i, b := range bad {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO queue_pending(project_id, mr_iid, job, ready_at, updated_at) VALUES (?, ?, ?, ?, 1)`,
			b.pid, b.iid, b.job, int64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC) // after the bad rows
	if err := s.SavePending(ctx, queue.Pending{Job: core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 9}, ReadyAt: at}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListPending(ctx)
	if err != nil || len(list) != 1 || list[0].Job.MRIID != 9 {
		t.Fatalf("list %+v %v", list, err)
	}
}
