package store

import (
	"context"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func seedRuns(t *testing.T, s *SQLite, base time.Time) {
	t.Helper()
	ctx := context.Background()
	runs := []meter.ClaudeRun{
		{At: base, Kind: "review", ProjectID: 1, ProjectPath: "g/api", MRIID: 7, MRTitle: "feat: old title", Username: "alice",
			Model: "", Usage: core.TokenUsage{Input: 10, Output: 20, CacheRead: 100, CacheWrite: 5}, CostUSD: 1, Duration: time.Second, NumTurns: 3, OK: true,
			Models: map[string]core.ModelUsage{
				"opus":  {TokenUsage: core.TokenUsage{Input: 8, Output: 15, CacheRead: 100, CacheWrite: 5}, CostUSD: 0.9},
				"haiku": {TokenUsage: core.TokenUsage{Input: 2, Output: 5}, CostUSD: 0.1},
			}},
		{At: base.Add(time.Hour), Kind: "chat", ProjectID: 1, ProjectPath: "g/api", MRIID: 7, MRTitle: "feat: new title", Username: "bob", UserID: 11,
			Model: "sonnet", Usage: core.TokenUsage{Input: 1, Output: 2}, CostUSD: 0.5, OK: true},
		{At: base.Add(25 * time.Hour), Kind: "review", ProjectID: 2, ProjectPath: "g/web", MRIID: 3, Username: "bob", UserID: 11,
			Model: "sonnet", Usage: core.TokenUsage{Input: 1000}, CostUSD: 2, OK: false, ErrorClass: "rate_limited"},
		{At: base.Add(26 * time.Hour), Kind: "other", Usage: core.TokenUsage{Output: 7}, OK: true},
	}
	for _, r := range runs {
		if err := s.RecordRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunAggregates(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	seedRuns(t, s, base)

	tot, err := s.RunTotals(ctx, meter.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if tot.Runs != 4 || tot.Failed != 1 || tot.Usage.Total() != 135+3+1000+7 || tot.CostUSD != 3.5 || tot.DurationMS != 1000 {
		t.Fatalf("totals: %+v", tot)
	}

	users, err := s.RunsBy(ctx, meter.RunFilter{}, meter.ByUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 || users[0].Key != "bob" || users[0].Runs != 2 || users[0].Usage.Total() != 1003 || users[0].ID != 11 ||
		users[1].Key != "alice" || users[1].Usage.Total() != 135 || users[2].Key != "" || users[2].Usage.Output != 7 {
		t.Fatalf("by user: %+v", users)
	}

	projects, _ := s.RunsBy(ctx, meter.RunFilter{}, meter.ByProject)
	if len(projects) != 3 || projects[0].Key != "g/web" || projects[0].ID != 2 || projects[1].Key != "g/api" || projects[1].Runs != 2 {
		t.Fatalf("by project: %+v", projects)
	}

	kinds, _ := s.RunsBy(ctx, meter.RunFilter{}, meter.ByKind)
	if len(kinds) != 3 || kinds[0].Key != "review" || kinds[0].Runs != 2 || kinds[0].Failed != 1 {
		t.Fatalf("by kind: %+v", kinds)
	}

	models, _ := s.RunsBy(ctx, meter.RunFilter{}, meter.ByModel)
	got := map[string]meter.RunGroup{}
	var sum int64
	for _, m := range models {
		got[m.Key] = m
		sum += m.Usage.Total()
	}
	if len(models) != 4 || got["opus"].Usage.Total() != 128 || got["haiku"].CostUSD != 0.1 || got["sonnet"].Runs != 2 ||
		got[""].Usage.Output != 7 || sum != tot.Usage.Total() {
		t.Fatalf("by model: %+v", models)
	}

	days, _ := s.RunsBy(ctx, meter.RunFilter{}, meter.ByDay)
	if len(days) != 2 || days[0].Key != "2026-10-01" || days[0].Runs != 2 || days[1].Key != "2026-10-02" || days[1].Runs != 2 {
		t.Fatalf("by day: %+v", days)
	}
	// Days follow the filter's time zone: 10:00 UTC + 15h -> next day.
	loc := time.FixedZone("X", 15*3600)
	days, _ = s.RunsBy(ctx, meter.RunFilter{Location: loc, Until: base.Add(48 * time.Hour)}, meter.ByDay)
	if len(days) != 2 || days[0].Key != "2026-10-02" || days[0].Runs != 2 {
		t.Fatalf("by day (tz): %+v", days)
	}

	mrs, err := s.TopMRs(ctx, meter.RunFilter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mrs) != 2 || mrs[0].ProjectPath != "g/web" || mrs[0].MRIID != 3 || mrs[1].MRIID != 7 || mrs[1].Runs != 2 ||
		mrs[1].Title != "feat: new title" || mrs[1].ProjectID != 1 {
		t.Fatalf("top mrs: %+v", mrs)
	}
	if mrs, _ := s.TopMRs(ctx, meter.RunFilter{}, 1); len(mrs) != 1 {
		t.Fatalf("limit: %+v", mrs)
	}
}

func TestRunFilters(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	seedRuns(t, s, base)

	tot, _ := s.RunTotals(ctx, meter.RunFilter{Since: base.Add(time.Hour), Until: base.Add(25 * time.Hour)})
	if tot.Runs != 1 || tot.Usage.Total() != 3 {
		t.Fatalf("time window: %+v", tot)
	}
	bob := "bob"
	tot, _ = s.RunTotals(ctx, meter.RunFilter{Username: &bob})
	if tot.Runs != 2 {
		t.Fatalf("user: %+v", tot)
	}
	none := ""
	tot, _ = s.RunTotals(ctx, meter.RunFilter{Username: &none})
	if tot.Runs != 1 || tot.Usage.Output != 7 {
		t.Fatalf("unattributed: %+v", tot)
	}
	tot, _ = s.RunTotals(ctx, meter.RunFilter{ProjectPath: "g/api", Username: &bob})
	if tot.Runs != 1 || tot.CostUSD != 0.5 {
		t.Fatalf("project+user: %+v", tot)
	}
	models, _ := s.RunsBy(ctx, meter.RunFilter{ProjectPath: "g/api"}, meter.ByModel)
	if len(models) != 3 {
		t.Fatalf("models in g/api: %+v", models)
	}
	mrs, _ := s.TopMRs(ctx, meter.RunFilter{Username: &bob, ProjectPath: "g/web"}, 5)
	if len(mrs) != 1 || mrs[0].MRIID != 3 {
		t.Fatalf("top mrs filtered: %+v", mrs)
	}
	empty, err := s.RunsBy(ctx, meter.RunFilter{ProjectPath: "nope"}, meter.ByUser)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty: %v %+v", err, empty)
	}
}

func TestPruneRuns(t *testing.T) {
	ctx := context.Background()
	s, path := openTest(t)
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	seedRuns(t, s, base)
	n, err := s.PruneRuns(ctx, base.Add(2*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
	tot, _ := s.RunTotals(ctx, meter.RunFilter{})
	if tot.Runs != 2 {
		t.Fatalf("after prune: %+v", tot)
	}
	var orphans int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claude_run_models WHERE run_id NOT IN (SELECT id FROM claude_runs)`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("orphan model rows: %d %v", orphans, err)
	}
	// Reopening the database keeps the data and the schema version.
	s.Close()
	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	tot, _ = s2.RunTotals(ctx, meter.RunFilter{})
	if tot.Runs != 2 {
		t.Fatalf("after reopen: %+v", tot)
	}
	var v int
	if err := s2.db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("schema_version = %d (%v), want %d", v, err, len(migrations))
	}
}
