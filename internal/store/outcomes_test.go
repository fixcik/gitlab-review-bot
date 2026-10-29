package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/learn"
)

func TestOutcomesCRUD(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	t0 := time.Now().Add(-time.Hour)

	a, err := s.RecordOutcome(ctx, learn.Outcome{ProjectPath: "g/a", MRIID: 7, FindingID: "f1", Title: "Нет ретраев", Severity: "major",
		Path: "client.go", Line: 12, BodyExcerpt: "body", HeadSHAAtPost: "abc123", CreatedAt: t0})
	if err != nil || a == 0 {
		t.Fatalf("record: %v %v", a, err)
	}
	b, _ := s.RecordOutcome(ctx, learn.Outcome{ProjectPath: "g/a", MRIID: 7, FindingID: "f2", CreatedAt: t0.Add(time.Minute)})
	c, _ := s.RecordOutcome(ctx, learn.Outcome{ProjectPath: "g/b", MRIID: 1, FindingID: "f1", Status: learn.OutcomeResolvedByBot})
	if _, err := s.RecordOutcome(ctx, learn.Outcome{ProjectPath: "g/a", FindingID: "x", Status: "weird"}); err == nil {
		t.Fatal("invalid status must fail")
	}
	if _, err := s.RecordOutcome(ctx, learn.Outcome{ProjectPath: "", FindingID: "x"}); err == nil {
		t.Fatal("missing project must fail")
	}

	list, err := s.ListOutcomes(ctx, learn.OutcomeFilter{ProjectPath: "G/A"})
	if err != nil || len(list) != 2 || list[0].ID != b || list[1].ID != a {
		t.Fatalf("list: %+v %v", list, err)
	}
	o := list[1]
	if o.Status != learn.OutcomeOpen || o.Title != "Нет ретраев" || o.Line != 12 || o.HeadSHAAtPost != "abc123" || o.BodyExcerpt != "body" ||
		o.Severity != "major" || o.Path != "client.go" || o.MRIID != 7 || !o.UsedAt.IsZero() || o.CreatedAt.IsZero() {
		t.Fatalf("outcome %+v", o)
	}

	// Bind + close by discussion; close by finding id while unbound.
	if err := s.SetOutcomeDiscussion(ctx, a, "d1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CloseOpenOutcome(ctx, "g/a", 7, "d1", "", learn.OutcomeFixed); err != nil || !ok {
		t.Fatalf("close by discussion: %v %v", ok, err)
	}
	if ok, _ := s.CloseOpenOutcome(ctx, "g/a", 7, "d1", "", learn.OutcomeFixed); ok {
		t.Fatal("already closed")
	}
	if ok, _ := s.CloseOpenOutcome(ctx, "g/a", 7, "dX", "f2", learn.OutcomeResolvedByBot); !ok {
		t.Fatal("close unbound by finding id")
	}
	if ok, _ := s.CloseOpenOutcome(ctx, "g/a", 7, "", "", learn.OutcomeFixed); ok {
		t.Fatal("nothing to match")
	}
	if err := s.SetOutcomeStatus(ctx, c, learn.OutcomeDismissed); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOutcomeStatus(ctx, 999, learn.OutcomeDismissed); err == nil {
		t.Fatal("unknown id")
	}
	if l, _ := s.ListOutcomes(ctx, learn.OutcomeFilter{Status: learn.OutcomeDismissed, Unused: true}); len(l) != 1 || l[0].ID != c {
		t.Fatalf("dismissed unused: %+v", l)
	}
	if err := s.MarkOutcomesUsed(ctx, []int64{c}, 42); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListOutcomes(ctx, learn.OutcomeFilter{Status: learn.OutcomeDismissed, Unused: true}); len(l) != 0 {
		t.Fatalf("used must be excluded: %+v", l)
	}
	if l, _ := s.ListOutcomes(ctx, learn.OutcomeFilter{ProjectPath: "g/b"}); l[0].RuleID != 42 || l[0].UsedAt.IsZero() {
		t.Fatalf("used: %+v", l[0])
	}
	if l, _ := s.ListOutcomes(ctx, learn.OutcomeFilter{ProjectPath: "g/a", Limit: 1}); len(l) != 1 {
		t.Fatal("limit")
	}

	if err := s.TouchOutcomes(ctx, "g/a", 7); err != nil {
		t.Fatal(err)
	}

	// Stats.
	st, err := s.OutcomeStats(ctx, "g/a", t0.Add(-time.Minute))
	if err != nil || st.Fixed != 1 || st.ResolvedByBot != 1 || st.Open != 0 || st.Dismissed != 0 {
		t.Fatalf("stats %+v %v", st, err)
	}
	if st, _ := s.OutcomeStats(ctx, "g/a", time.Now().Add(time.Hour)); st != (learn.OutcomeStats{}) {
		t.Fatalf("window: %+v", st)
	}
	if p, ok := (learn.OutcomeStats{Fixed: 3, Dismissed: 1}).Precision(); !ok || p != 0.75 {
		t.Fatalf("precision %v %v", p, ok)
	}
	if _, ok := (learn.OutcomeStats{Open: 2}).Precision(); ok {
		t.Fatal("no precision without fixed+dismissed")
	}
}

func TestSynthRules(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if err := s.AddSynthRule(ctx, learn.SynthRule{RuleID: 5, ProjectPath: "g/a", MRIID: 7, Dismissed: 3}); err != nil {
		t.Fatal(err)
	}
	s.AddSynthRule(ctx, learn.SynthRule{RuleID: 6, ProjectPath: "g/a", MRIID: 8, Dismissed: 4})
	p, err := s.PendingSynthRules(ctx, "G/A", 7, 2)
	if err != nil || len(p) != 1 || p[0].RuleID != 5 || p[0].Dismissed != 3 {
		t.Fatalf("pending %+v %v", p, err)
	}
	s.MarkSynthRulesShown(ctx, []int64{5})
	s.MarkSynthRulesShown(ctx, []int64{5})
	if p, _ := s.PendingSynthRules(ctx, "g/a", 7, 2); len(p) != 0 {
		t.Fatalf("shown twice: %+v", p)
	}
	m, err := s.SynthRuleSources(ctx, []int64{5, 6, 7})
	if err != nil || len(m) != 2 || m[5] != 3 || m[6] != 4 {
		t.Fatalf("sources %v %v", m, err)
	}
	if m, _ := s.SynthRuleSources(ctx, nil); len(m) != 0 {
		t.Fatal("empty")
	}
}

// TestOutcomesMigrationOnExistingDB: a database from before this feature
// (sequential migration + learnings_v1 recorded) gets the outcomes tables
// from the named step, keeps its data, and re-opening is a no-op.
func TestOutcomesMigrationOnExistingDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL); INSERT INTO schema_version(version) VALUES (1);` + migrations[0] +
		`; CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL);` + namedMigrations[0].sql +
		`; INSERT INTO schema_migrations(name, applied_at) VALUES ('learnings_v1', 1);
		 INSERT INTO learnings(scope, project_path, rule, rule_key, created_at, updated_at) VALUES ('global', 'g/a', 'Старое правило', 'старое правило', 1, 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	if r, _ := s.GetLearning(ctx, 1); r == nil || r.Text != "Старое правило" {
		t.Fatalf("old rule lost: %+v", r)
	}
	if _, err := s.RecordOutcome(ctx, learn.Outcome{ProjectPath: "g/a", MRIID: 1, FindingID: "f"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE name = 'finding_outcomes_v1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("schema_migrations %d %v", n, err)
	}
	s.Close()
	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if l, _ := s2.ListOutcomes(ctx, learn.OutcomeFilter{}); len(l) != 1 {
		t.Fatalf("outcome lost on reopen: %+v", l)
	}
}

func TestOutcomeRejectedByVerifier(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if _, err := s.RecordOutcome(ctx, learn.Outcome{ProjectPath: "g/a", MRIID: 1, FindingID: "f1", Status: learn.OutcomeRejectedByVerifier}); err != nil {
		t.Fatal(err)
	}
	st, err := s.OutcomeStats(ctx, "g/a", time.Time{})
	if err != nil || st.RejectedByVerifier != 1 || st.Open != 0 {
		t.Fatalf("stats %+v %v", st, err)
	}
	if _, ok := st.Precision(); ok {
		t.Error("rejections are not part of the precision")
	}
}
