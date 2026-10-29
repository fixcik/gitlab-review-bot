package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMRPauseCRUD(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	t0 := time.Date(2026, 10, 24, 9, 30, 0, 0, time.UTC)

	if p, by, at, err := s.GetMRPause(ctx, 1, 2); err != nil || p || by != "" || !at.IsZero() {
		t.Fatalf("empty: %v %q %v %v", p, by, at, err)
	}
	if was, err := s.ClearMRPause(ctx, 1, 2); err != nil || was {
		t.Fatalf("clear unpaused: %v %v", was, err)
	}
	if err := s.SetMRPause(ctx, 1, 2, "bob", t0); err != nil {
		t.Fatal(err)
	}
	if p, by, at, err := s.GetMRPause(ctx, 1, 2); err != nil || !p || by != "bob" || !at.Equal(t0) {
		t.Fatalf("paused: %v %q %v %v", p, by, at, err)
	}
	// Other MRs are not affected.
	if p, _, _, _ := s.GetMRPause(ctx, 1, 3); p {
		t.Fatal("other mr paused")
	}
	if p, _, _, _ := s.GetMRPause(ctx, 2, 2); p {
		t.Fatal("other project paused")
	}
	// Overwrite.
	if err := s.SetMRPause(ctx, 1, 2, "eve", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, by, at, _ := s.GetMRPause(ctx, 1, 2); by != "eve" || !at.Equal(t0.Add(time.Hour)) {
		t.Fatalf("overwrite: %q %v", by, at)
	}
	if was, err := s.ClearMRPause(ctx, 1, 2); err != nil || !was {
		t.Fatalf("clear: %v %v", was, err)
	}
	if p, by, _, err := s.GetMRPause(ctx, 1, 2); err != nil || p || by != "" {
		t.Fatalf("after clear: %v %q %v", p, by, err)
	}
	if was, _ := s.ClearMRPause(ctx, 1, 2); was {
		t.Fatal("second clear must report not paused")
	}
	// Pause again after a clear.
	if err := s.SetMRPause(ctx, 1, 2, "bob", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if p, _, at, _ := s.GetMRPause(ctx, 1, 2); !p || at.IsZero() {
		t.Fatalf("re-pause: %v %v", p, at)
	}
}

// TestMRFlagsMigrationOnExistingDB: a database created before the feature
// (sequential migrations + learnings_v1 recorded) gets the mr_flags table
// from the named step, keeps its data, and re-opening is a no-op.
func TestMRFlagsMigrationOnExistingDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL); INSERT INTO schema_version(version) VALUES (1);` + migrations[0] +
		`; INSERT INTO mr_state(project_id, mr_iid, reviewed_sha) VALUES (1, 2, 'abc');
		CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	for _, m := range namedMigrations {
		if m.name == "mr_flags_v1" {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(name, applied_at) VALUES (?, 1)`, m.name); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	if st, err := s.GetMRState(ctx, 1, 2); err != nil || st == nil || st.ReviewedSHA != "abc" {
		t.Fatalf("old data lost: %+v %v", st, err)
	}
	if err := s.SetMRPause(ctx, 1, 2, "bob", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if p, by, _, err := s2.GetMRPause(ctx, 1, 2); err != nil || !p || by != "bob" {
		t.Fatalf("pause lost on reopen: %v %q %v", p, by, err)
	}
	var n int
	if err := s2.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE name = 'mr_flags_v1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("schema_migrations rows %d %v", n, err)
	}
}
