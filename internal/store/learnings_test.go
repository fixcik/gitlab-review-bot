package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
)

func TestLearningsCRUD(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	a, created, err := s.AddLearning(ctx, learn.Rule{Scope: "project", ProjectPath: "g/a", Text: "Не требуй ретраев для вызовов через API-шлюз.",
		SourceURL: "https://gl/g/a/-/merge_requests/1#note_5", SourceNoteID: 5, Author: "bob", CreatedAt: t0})
	if err != nil || !created || a.ID == 0 {
		t.Fatalf("add: %+v %v %v", a, created, err)
	}
	if a.Scope != learn.ScopeProject || a.ProjectPath != "g/a" || a.Author != "bob" || a.SourceNoteID != 5 || !a.CreatedAt.Equal(t0) {
		t.Fatalf("saved %+v", a)
	}
	g, _, err := s.AddLearning(ctx, learn.Rule{Scope: "global", ProjectPath: "g/b", Text: "Логи пиши на английском.", Author: "eve", CreatedAt: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.AddLearning(ctx, learn.Rule{Scope: "project", ProjectPath: "g/c", Text: "Только для C.", CreatedAt: t0.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	// Get.
	got, err := s.GetLearning(ctx, a.ID)
	if err != nil || got == nil || got.Text != a.Text {
		t.Fatalf("get: %+v %v", got, err)
	}
	if missing, err := s.GetLearning(ctx, 9999); err != nil || missing != nil {
		t.Fatalf("get missing: %+v %v", missing, err)
	}

	// List for g/a: own + global, newest first; g/c's rule is not included.
	list, err := s.ListLearnings(ctx, "G/A", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != g.ID || list[1].ID != a.ID {
		t.Fatalf("list %+v", list)
	}
	if l, _ := s.ListLearnings(ctx, "g/a", 1); len(l) != 1 || l[0].ID != g.ID {
		t.Fatalf("limit: %+v", l)
	}
	if l, _ := s.ListLearnings(ctx, "g/c", 0); len(l) != 2 || l[0].ID != other.ID {
		t.Fatalf("list c: %+v", l)
	}

	// Scope change.
	if err := s.SetLearningScope(ctx, a.ID, learn.ScopeGlobal, "g/a"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListLearnings(ctx, "g/c", 0); len(l) != 3 {
		t.Fatalf("global rule should apply to g/c: %+v", l)
	}
	if err := s.SetLearningScope(ctx, 9999, learn.ScopeGlobal, "x"); err == nil {
		t.Fatal("scope of unknown rule should fail")
	}

	// Soft delete.
	if err := s.DeleteLearning(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetLearning(ctx, a.ID); got != nil {
		t.Fatalf("deleted rule still visible: %+v", got)
	}
	if l, _ := s.ListLearnings(ctx, "g/a", 0); len(l) != 1 || l[0].ID != g.ID {
		t.Fatalf("list after delete: %+v", l)
	}
	if err := s.SetLearningScope(ctx, a.ID, learn.ScopeProject, "g/a"); err == nil {
		t.Fatal("scope of deleted rule should fail")
	}
	var deletedAt sql.NullInt64
	if err := s.db.QueryRow(`SELECT deleted_at FROM learnings WHERE id = ?`, a.ID).Scan(&deletedAt); err != nil || !deletedAt.Valid {
		t.Fatalf("row should stay with deleted_at set: %v %v", deletedAt, err)
	}
}

func TestLearningsDedupe(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	first, created, err := s.AddLearning(ctx, learn.Rule{Scope: "project", ProjectPath: "g/a", Text: "Не требуй ретраев.", SourceNoteID: 1, Author: "bob", CreatedAt: t0})
	if err != nil || !created {
		t.Fatal(err)
	}
	// Same text modulo case/spaces/punctuation -> update.
	again, created, err := s.AddLearning(ctx, learn.Rule{Scope: "project", ProjectPath: "G/A", Text: "  не   требуй ретраев ", SourceNoteID: 2, Author: "eve", CreatedAt: t0.Add(time.Hour)})
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("dedupe: %+v created=%v err=%v", again, created, err)
	}
	if again.Author != "eve" || again.SourceNoteID != 2 || !again.CreatedAt.Equal(t0) || !again.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("update: %+v", again)
	}
	// Other project or other scope -> new rows.
	if r, created, _ := s.AddLearning(ctx, learn.Rule{Scope: "project", ProjectPath: "g/b", Text: "Не требуй ретраев."}); !created || r.ID == first.ID {
		t.Fatal("other project must be a new rule")
	}
	gl, created, _ := s.AddLearning(ctx, learn.Rule{Scope: "global", ProjectPath: "g/a", Text: "Не требуй ретраев."})
	if !created {
		t.Fatal("other scope must be a new rule")
	}
	// Global dedupe ignores the project.
	if r, created, _ := s.AddLearning(ctx, learn.Rule{Scope: "global", ProjectPath: "g/z", Text: "не требуй ретраев"}); created || r.ID != gl.ID {
		t.Fatalf("global dedupe: %+v %v", r, created)
	}
	// A deleted rule does not block re-learning.
	if err := s.DeleteLearning(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if r, created, _ := s.AddLearning(ctx, learn.Rule{Scope: "project", ProjectPath: "g/a", Text: "Не требуй ретраев."}); !created || r.ID == first.ID {
		t.Fatal("re-learning a deleted rule must insert")
	}
	if _, _, err := s.AddLearning(ctx, learn.Rule{Scope: "project", Text: " ... "}); err == nil {
		t.Fatal("empty rule must fail")
	}
}

func TestProcessedNotes(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if ok, err := s.MarkNoteProcessed(ctx, 42); err != nil || !ok {
		t.Fatalf("first: %v %v", ok, err)
	}
	if ok, err := s.MarkNoteProcessed(ctx, 42); err != nil || ok {
		t.Fatalf("second: %v %v", ok, err)
	}
	if ok, _ := s.MarkNoteProcessed(ctx, 43); !ok {
		t.Fatal("other note")
	}
}

// TestLearningsMigrationOnExistingDB opens a database created by the
// previous schema (sequential migrations only, with data) and checks that
// the named migration adds the tables without touching the old data, and
// that re-opening is a no-op.
func TestLearningsMigrationOnExistingDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL); INSERT INTO schema_version(version) VALUES (1);` + migrations[0] +
		`; INSERT INTO mr_state(project_id, mr_iid, reviewed_sha) VALUES (1, 2, 'abc')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	if st, err := s.GetMRState(ctx, 1, 2); err != nil || st == nil || st.ReviewedSHA != "abc" {
		t.Fatalf("old data lost: %+v %v", st, err)
	}
	r, _, err := s.AddLearning(ctx, learn.Rule{Scope: "global", Text: "Правило."})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(ctx, core.Usage{Kind: core.UsageLearn, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("schema_version %d %v", version, err)
	}
	s.Close()

	// Re-open: nothing re-applied, data kept.
	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got, _ := s2.GetLearning(ctx, r.ID); got == nil {
		t.Fatal("rule lost on reopen")
	}
	var n int
	if err := s2.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE name = 'learnings_v1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("schema_migrations rows %d %v", n, err)
	}
}

// TestNamedMigrationTablesAlreadyExist: the step is idempotent even when the
// tables exist but the step is not recorded (e.g. created by another branch).
func TestNamedMigrationTablesAlreadyExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("re-applying the named step must be a no-op: %v", err)
	}
	s.Close()
}
