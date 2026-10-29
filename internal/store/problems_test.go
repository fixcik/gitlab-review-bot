package store

import (
	"context"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
)

func TestProblemsUpsertAckReappearPrune(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	t0 := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	rec := func(fp string, kind problems.Kind, project, subject, detail string, at time.Time, mr int64) problems.Record {
		return problems.Record{Fingerprint: fp, Kind: kind, Source: problems.SourceBot, ProjectPath: project, Subject: subject,
			Detail: detail, FirstSeen: at, LastSeen: at, LastMRIID: mr, LastRunID: "run-" + fp}
	}
	fpA := problems.Fingerprint(problems.LintFailed, "g/api", "golangci-lint: упал")
	up, err := s.UpsertProblem(ctx, rec(fpA, problems.LintFailed, "g/api", "golangci-lint: упал", "exit 3", t0, 7))
	if err != nil || !up.New || up.Reappeared || !up.NotifiedAt.IsZero() {
		t.Fatalf("insert: %+v %v", up, err)
	}
	// Seen again: count, detail, last MR/run/seen updated; first seen and
	// the subject's first spelling kept.
	r2 := rec(fpA, problems.LintFailed, "g/api", "GOLANGCI-LINT: упал", "exit 4", t0.Add(time.Hour), 8)
	r2.LastRunID = "run-2"
	if up, err = s.UpsertProblem(ctx, r2); err != nil || up.New || up.Reappeared {
		t.Fatalf("update: %+v %v", up, err)
	}
	rows, err := s.ListProblems(ctx, problems.Filter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v %v", rows, err)
	}
	r := rows[0]
	if r.Count != 2 || r.Detail != "exit 4" || r.Subject != "golangci-lint: упал" || !r.FirstSeen.Equal(t0) ||
		!r.LastSeen.Equal(t0.Add(time.Hour)) || r.LastMRIID != 8 || r.LastRunID != "run-2" || r.Kind != problems.LintFailed ||
		r.Source != problems.SourceBot || !r.AckedAt.IsZero() {
		t.Fatalf("row %+v", r)
	}
	// Notified.
	if err := s.MarkProblemsNotified(ctx, []string{fpA, "unknown"}, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkProblemsNotified(ctx, nil, t0); err != nil {
		t.Fatal(err)
	}
	// Ack hides it; a second ack is a no-op.
	if ok, err := s.AckProblem(ctx, fpA, t0.Add(3*time.Hour)); err != nil || !ok {
		t.Fatalf("ack: %v %v", ok, err)
	}
	if ok, _ := s.AckProblem(ctx, fpA, t0.Add(3*time.Hour)); ok {
		t.Fatal("second ack")
	}
	if ok, _ := s.AckProblem(ctx, "nope", t0); ok {
		t.Fatal("ack of an unknown row")
	}
	if rows, _ := s.ListProblems(ctx, problems.Filter{Status: problems.StatusNew}); len(rows) != 0 {
		t.Fatalf("acked row listed as new: %+v", rows)
	}
	if rows, _ := s.ListProblems(ctx, problems.Filter{Status: problems.StatusAcked}); len(rows) != 1 || rows[0].AckedAt.IsZero() ||
		!rows[0].NotifiedAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("acked: %+v", rows)
	}
	// Re-appears: unacknowledged again, reported as such with the last
	// notification time.
	up, err = s.UpsertProblem(ctx, rec(fpA, problems.LintFailed, "g/api", "golangci-lint: упал", "exit 5", t0.Add(4*time.Hour), 9))
	if err != nil || up.New || !up.Reappeared || !up.NotifiedAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("reappear: %+v %v", up, err)
	}
	if rows, _ := s.ListProblems(ctx, problems.Filter{Status: problems.StatusNew}); len(rows) != 1 || rows[0].Count != 3 {
		t.Fatalf("after reappear: %+v", rows)
	}
	// Filters, order (newest last_seen first), limit.
	fpB := problems.Fingerprint(problems.ContextUnavailable, "g/web", "g/payments")
	if _, err := s.UpsertProblem(ctx, rec(fpB, problems.ContextUnavailable, "g/web", "g/payments", "", t0.Add(5*time.Hour), 1)); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.ListProblems(ctx, problems.Filter{}); len(rows) != 2 || rows[0].Fingerprint != fpB {
		t.Fatalf("order: %+v", rows)
	}
	if rows, _ := s.ListProblems(ctx, problems.Filter{Kind: problems.LintFailed}); len(rows) != 1 || rows[0].Fingerprint != fpA {
		t.Fatalf("kind filter: %+v", rows)
	}
	if rows, _ := s.ListProblems(ctx, problems.Filter{Project: "G/WEB"}); len(rows) != 1 || rows[0].Fingerprint != fpB {
		t.Fatalf("project filter: %+v", rows)
	}
	if rows, _ := s.ListProblems(ctx, problems.Filter{Limit: 1}); len(rows) != 1 {
		t.Fatalf("limit: %+v", rows)
	}
	// Prune: rows not seen since before.
	if n, err := s.PruneProblems(ctx, t0.Add(4*time.Hour+time.Minute)); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if rows, _ := s.ListProblems(ctx, problems.Filter{}); len(rows) != 1 || rows[0].Fingerprint != fpB {
		t.Fatalf("after prune: %+v", rows)
	}
	if _, err := s.UpsertProblem(ctx, problems.Record{}); err == nil {
		t.Fatal("empty record accepted")
	}
}

// The service on top of the real store: problems survive a reopen and the
// named migration is recorded.
func TestProblemsServiceOnSQLite(t *testing.T) {
	ctx := context.Background()
	s, path := openTest(t)
	svc := problems.New(problems.Options{Store: s, Notify: []string{"alice"}})
	c, r := svc.Start(ctx, core.Job{Kind: core.JobAutoReview, ProjectID: 1, ProjectPath: "g/api", MRIID: 3})
	problems.Add(c, problems.Problem{Kind: problems.DiffTruncated, Subject: "дифф больше лимитов"})
	outs := problems.Commit(c)
	if len(outs) != 1 || !outs[0].New || !outs[0].Notify {
		t.Fatalf("outs %+v", outs)
	}
	svc.MarkNotified(c, outs)
	svc.Finish(c, r)
	s.Close()
	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rows, err := s2.ListProblems(ctx, problems.Filter{})
	if err != nil || len(rows) != 1 || rows[0].NotifiedAt.IsZero() || rows[0].ProjectPath != "g/api" || rows[0].LastMRIID != 3 {
		t.Fatalf("after reopen: %+v %v", rows, err)
	}
	var n int
	if err := s2.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE name = 'problems_v1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("migration recorded: %d %v", n, err)
	}
}

// A problem the bot found fixed is "resolved": listed apart from the active
// and the hidden ones, and active again when it is seen again.
func TestProblemsResolvedAndReappear(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	t0 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	fp := problems.Fingerprint(problems.RepoConfigInvalid, "g/api", ".reviewbot.yaml")
	rec := problems.Record{Fingerprint: fp, Kind: problems.RepoConfigInvalid, Source: problems.SourceBot, ProjectPath: "g/api",
		Subject: ".reviewbot.yaml", Detail: "line 4", FirstSeen: t0, LastSeen: t0}
	if _, err := s.UpsertProblem(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ResolveProblem(ctx, "nope", t0); err != nil || ok {
		t.Fatalf("resolve of an unknown row: %v %v", ok, err)
	}
	if ok, err := s.ResolveProblem(ctx, fp, t0.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("resolve: %v %v", ok, err)
	}
	if ok, _ := s.ResolveProblem(ctx, fp, t0.Add(2*time.Hour)); ok {
		t.Fatal("a second resolve changed the row")
	}
	count := func(status string) int {
		rows, err := s.ListProblems(ctx, problems.Filter{Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if count(problems.StatusNew) != 0 || count(problems.StatusResolved) != 1 || count(problems.StatusAcked) != 0 || count("") != 1 {
		t.Fatalf("new %d resolved %d acked %d all %d", count(problems.StatusNew), count(problems.StatusResolved), count(problems.StatusAcked), count(""))
	}
	rows, _ := s.ListProblems(ctx, problems.Filter{})
	if !rows[0].ResolvedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("resolved at %v", rows[0].ResolvedAt)
	}
	// Seen again: active, and reported as reappeared (people are told again).
	up, err := s.UpsertProblem(ctx, rec)
	if err != nil || up.New || !up.Reappeared {
		t.Fatalf("reappear: %+v %v", up, err)
	}
	if count(problems.StatusNew) != 1 || count(problems.StatusResolved) != 0 {
		t.Fatalf("after reappearing: new %d resolved %d", count(problems.StatusNew), count(problems.StatusResolved))
	}
}

// "Вернуть" on /problems: a hidden problem is active again; un-hiding an
// active or unknown row is a no-op; errors of a closed store surface.
func TestProblemsUnack(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	t0 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	fp := problems.Fingerprint(problems.LabelsFailed, "g/api", "метка")
	if _, err := s.UpsertProblem(ctx, problems.Record{Fingerprint: fp, Kind: problems.LabelsFailed, ProjectPath: "g/api",
		Subject: "метка", FirstSeen: t0, LastSeen: t0}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UnackProblem(ctx, fp); err != nil || ok {
		t.Fatalf("unack of an active row: %v %v", ok, err)
	}
	if ok, err := s.AckProblem(ctx, fp, t0.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("ack: %v %v", ok, err)
	}
	if ok, err := s.UnackProblem(ctx, fp); err != nil || !ok {
		t.Fatalf("unack: %v %v", ok, err)
	}
	if rows, _ := s.ListProblems(ctx, problems.Filter{Status: problems.StatusNew}); len(rows) != 1 || !rows[0].AckedAt.IsZero() || rows[0].Count != 1 {
		t.Fatalf("after unack: %+v", rows)
	}
	if ok, _ := s.UnackProblem(ctx, "nope"); ok {
		t.Fatal("unack of an unknown row")
	}
	// Hidden again later: the ack works as before.
	if ok, _ := s.AckProblem(ctx, fp, t0.Add(2*time.Hour)); !ok {
		t.Fatal("ack after unack")
	}
	s.Close()
	if _, err := s.UnackProblem(ctx, fp); err == nil {
		t.Fatal("closed store: no error")
	}
}
