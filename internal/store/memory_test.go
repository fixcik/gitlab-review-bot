package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/memory"
)

func TestMemoryCRUD(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	if got, err := s.ListMemory(ctx, 42); err != nil || len(got) != 0 {
		t.Fatalf("empty: %+v %v", got, err)
	}
	if err := s.SaveMemorySection(ctx, memory.Section{ProjectID: 42, ProjectPath: "g/api", Key: memory.SectionTest,
		Content: "- `make test-unit`\n", UpdatedAt: t0, UpdatedByMR: 7, SourceNote: "ревью !7"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMemorySection(ctx, memory.Section{ProjectID: 42, ProjectPath: "g/api", Key: memory.SectionBuild, Content: "- `make build`", UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMemorySection(ctx, memory.Section{ProjectID: 43, ProjectPath: "g/other", Key: memory.SectionTest, Content: "- other"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListMemory(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	// Display order: build before test; other projects never leak in.
	if len(got) != 2 || got[0].Key != memory.SectionBuild || got[1].Key != memory.SectionTest {
		t.Fatalf("list %+v", got)
	}
	tst := got[1]
	if tst.ProjectID != 42 || tst.ProjectPath != "g/api" || tst.Content != "- `make test-unit`\n" || !tst.UpdatedAt.Equal(t0) || tst.UpdatedByMR != 7 || tst.SourceNote != "ревью !7" {
		t.Fatalf("section %+v", tst)
	}
	// Replace.
	if err := s.SaveMemorySection(ctx, memory.Section{ProjectID: 42, ProjectPath: "g/api", Key: memory.SectionTest, Content: "- `make test`", UpdatedByMR: 8}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListMemory(ctx, 42)
	if len(got) != 2 || got[1].Content != "- `make test`\n" || got[1].UpdatedByMR != 8 {
		t.Fatalf("replace %+v", got)
	}
	// Empty content deletes.
	if err := s.SaveMemorySection(ctx, memory.Section{ProjectID: 42, Key: memory.SectionBuild, Content: "  "}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ListMemory(ctx, 42); len(got) != 1 {
		t.Fatalf("delete via empty: %+v", got)
	}
	// Validation.
	if err := s.SaveMemorySection(ctx, memory.Section{ProjectID: 42, Key: "weather", Content: "x"}); err == nil {
		t.Fatal("unknown section must fail")
	}
	if err := s.SaveMemorySection(ctx, memory.Section{Key: memory.SectionTest, Content: "x"}); err == nil {
		t.Fatal("zero project id must fail")
	}
	// Clear one / all.
	s.SaveMemorySection(ctx, memory.Section{ProjectID: 42, Key: memory.SectionLint, Content: "- lint"})
	if n, err := s.ClearMemory(ctx, 42, memory.SectionLint); err != nil || n != 1 {
		t.Fatalf("clear section: %d %v", n, err)
	}
	if n, _ := s.ClearMemory(ctx, 42, memory.SectionLint); n != 0 {
		t.Fatal("clear again")
	}
	if n, err := s.ClearMemory(ctx, 42, ""); err != nil || n != 1 {
		t.Fatalf("clear all: %d %v", n, err)
	}
	if got, _ := s.ListMemory(ctx, 43); len(got) != 1 {
		t.Fatal("other project must keep its memory")
	}
}

func TestMemoryHardCap(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	big := strings.Repeat("я", memory.HardSectionCap*2)
	if err := s.SaveMemorySection(ctx, memory.Section{ProjectID: 1, Key: memory.SectionOther, Content: big}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListMemory(ctx, 1)
	if n := len([]rune(strings.TrimSpace(got[0].Content))); n != memory.HardSectionCap {
		t.Fatalf("stored %d runes", n)
	}
}

// TestMemoryKeyedByProjectID: a renamed / moved repository keeps its memory
// (same numeric project ID), and the display path follows the new path.
func TestMemoryKeyedByProjectID(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	s.SaveMemorySection(ctx, memory.Section{ProjectID: 42, ProjectPath: "old/api", Key: memory.SectionTest, Content: "- t"})
	s.SaveMemorySection(ctx, memory.Section{ProjectID: 42, ProjectPath: "old/api", Key: memory.SectionLint, Content: "- l"})

	if err := s.SetMemoryProjectPath(ctx, 42, "new-group/api-v2"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListMemory(ctx, 42)
	if len(got) != 2 || got[0].ProjectPath != "new-group/api-v2" || got[1].ProjectPath != "new-group/api-v2" {
		t.Fatalf("after move %+v", got)
	}
	// A save with another path refreshes every row of the project.
	s.SaveMemorySection(ctx, memory.Section{ProjectID: 42, ProjectPath: "third/api", Key: memory.SectionBuild, Content: "- b"})
	got, _ = s.ListMemory(ctx, 42)
	for _, m := range got {
		if m.ProjectPath != "third/api" {
			t.Fatalf("path not refreshed: %+v", m)
		}
	}
	if err := s.SetMemoryProjectPath(ctx, 42, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ListMemory(ctx, 42); got[0].ProjectPath != "third/api" {
		t.Fatal("empty path must not wipe the display path")
	}
}

// TestMemoryMigrationOnExistingDB: a database from before this feature
// (sequential migrations + learnings_v1 recorded) gets project_memory from
// the named step without losing data; re-opening is a no-op.
func TestMemoryMigrationOnExistingDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL); INSERT INTO schema_version(version) VALUES (1);` + migrations[0] +
		`; INSERT INTO mr_state(project_id, mr_iid, reviewed_sha) VALUES (1, 2, 'abc');
		CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL);` + namedMigrations[0].sql +
		`; INSERT INTO schema_migrations(name, applied_at) VALUES ('learnings_v1', 1);`); err != nil {
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
	if err := s.SaveMemorySection(ctx, memory.Section{ProjectID: 1, ProjectPath: "g/p", Key: memory.SectionTest, Content: "- t"}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got, _ := s2.ListMemory(ctx, 1); len(got) != 1 {
		t.Fatal("memory lost on reopen")
	}
	var n int
	if err := s2.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE name = 'project_memory_v1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("schema_migrations rows %d %v", n, err)
	}
}
