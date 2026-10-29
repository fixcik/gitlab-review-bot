package store

import (
	"context"
	"database/sql"
	"github.com/fixcik/gitlab-review-bot/internal/diag/diagindex"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"path/filepath"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

var allTables = []string{
	"mr_state", "usage", "schema_version", "schema_migrations",
	"learnings", "learn_processed_notes", "mr_flags", "finding_outcomes", "outcome_rules",
	"project_memory", "diag_runs", "claude_runs", "claude_run_models", "queue_pending", "queue_jobs",
	"poll_cursors", "poll_mrs", "sysmap_repos", "sysmap_files", "sysmap_runs",
}

// exerciseAll touches every feature's table through the public API.
func exerciseAll(t *testing.T, s *SQLite) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := s.SaveMRState(ctx, core.MRState{ProjectID: 1, MRIID: 2, ReviewedSHA: "abc"}); err != nil {
		t.Fatalf("mr_state: %v", err)
	}
	if err := s.RecordUsage(ctx, core.Usage{Kind: core.UsageAuto, MRKey: "1!2", At: now}); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if _, _, err := s.AddLearning(ctx, learn.Rule{Scope: learn.ScopeGlobal, Text: "rule text " + now.String(), CreatedAt: now}); err != nil {
		t.Fatalf("learnings: %v", err)
	}
	if _, err := s.MarkNoteProcessed(ctx, now.UnixNano()); err != nil {
		t.Fatalf("learn_processed_notes: %v", err)
	}
	if err := s.SetMRPause(ctx, 1, 2, "bob", now); err != nil {
		t.Fatalf("mr_flags: %v", err)
	}
	if _, err := s.RecordOutcome(ctx, learn.Outcome{ProjectPath: "g/p", MRIID: 2, FindingID: "f", Status: learn.OutcomeOpen, CreatedAt: now}); err != nil {
		t.Fatalf("finding_outcomes: %v", err)
	}
	if err := s.AddSynthRule(ctx, learn.SynthRule{RuleID: now.UnixNano(), ProjectPath: "g/p", MRIID: 2, CreatedAt: now}); err != nil {
		t.Fatalf("outcome_rules: %v", err)
	}
	if err := s.SaveMemorySection(ctx, memory.Section{ProjectID: 1, ProjectPath: "g/p", Key: "test", Content: "- go test ./...", UpdatedAt: now}); err != nil {
		t.Fatalf("project_memory: %v", err)
	}
	if err := s.SavePending(ctx, queue.Pending{Job: core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 2}, ReadyAt: now}); err != nil {
		t.Fatalf("queue_pending: %v", err)
	}
	if _, err := s.AddJob(ctx, queue.Stored{Job: core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 2, EnqueuedAt: now}, ReadyAt: now}); err != nil {
		t.Fatalf("queue_jobs: %v", err)
	}
	if err := s.SaveDiagRun(ctx, diagindex.Row{ID: "r" + now.Format("150405.000000000"), StartedAt: now, Kind: "review"}); err != nil {
		t.Fatalf("diag_runs: %v", err)
	}
	if err := s.RecordRun(ctx, meter.ClaudeRun{At: now, Kind: "review", OK: true}); err != nil {
		t.Fatalf("claude_runs: %v", err)
	}
	if err := s.SetPollCursor(ctx, "g", now); err != nil {
		t.Fatalf("poll_cursors: %v", err)
	}
	if err := s.SavePollMR(ctx, events.PollMR{ProjectID: 1, IID: 2, State: "opened", UpdatedAt: now}); err != nil {
		t.Fatalf("poll_mrs: %v", err)
	}
	if err := s.SaveSysmapRepo(ctx, sysmap.RepoState{Path: "g/p", SHA: "abc", Status: sysmap.StatusOK}); err != nil {
		t.Fatalf("sysmap_repos: %v", err)
	}
	if err := s.SaveSysmapFiles(ctx, "g/p", []sysmap.FileRecord{{Path: "a.py", Hash: "h", Version: sysmap.CodeVersion}}); err != nil {
		t.Fatalf("sysmap_files: %v", err)
	}
	if err := s.RecordSysmapRun(ctx, sysmap.RunRecord{At: now, Repo: "g/p", OK: true}); err != nil {
		t.Fatalf("sysmap_runs: %v", err)
	}
	for _, tbl := range allTables {
		var name string
		if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", tbl, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(namedMigrations) {
		t.Errorf("schema_migrations rows = %d (err %v), want %d", n, err, len(namedMigrations))
	}
}

func TestMigrationsFreshDB(t *testing.T) {
	s, path := openTest(t)
	exerciseAll(t, s)
	s.Close()
	// Reopen: every migration is idempotent.
	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	exerciseAll(t, s2)
}

// An old database: only the sequential schema (first release) and the first
// named migration, without the schema_migrations rows of later features; one
// table of a later feature already exists (created by an earlier build of a
// branch) without its schema_migrations row.
func TestMigrationsOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(version) VALUES (1)`,
		migrations[0],
		`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		namedMigrations[0].sql,
		`INSERT INTO schema_migrations(name, applied_at) VALUES ('` + namedMigrations[0].name + `', 1)`,
		`CREATE TABLE mr_flags (project_id INTEGER NOT NULL, mr_iid INTEGER NOT NULL, paused INTEGER NOT NULL DEFAULT 0,
			paused_by TEXT NOT NULL DEFAULT '', paused_at INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (project_id, mr_iid))`,
		`INSERT INTO mr_state(project_id, mr_iid, reviewed_sha) VALUES (9, 9, 'old')`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	db.Close()

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	defer s.Close()
	st, err := s.GetMRState(context.Background(), 9, 9)
	if err != nil || st == nil || st.ReviewedSHA != "old" {
		t.Fatalf("old data lost: %+v %v", st, err)
	}
	exerciseAll(t, s)
}
