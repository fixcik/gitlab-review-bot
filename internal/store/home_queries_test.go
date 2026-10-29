package store

import (
	"context"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag/diagindex"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
)

func TestRunsHourly(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	since := time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC)
	until := since.Add(3*time.Hour + 20*time.Minute) // 4 hours, the last one partial
	for _, r := range []meter.ClaudeRun{
		{At: since.Add(-time.Nanosecond), CostUSD: 100, OK: true},                                         // before the window
		{At: since, CostUSD: 1, Usage: core.TokenUsage{Input: 10}, OK: true},                              // hour 0 (inclusive start)
		{At: since.Add(59 * time.Minute), CostUSD: 2, OK: false},                                          // hour 0
		{At: since.Add(time.Hour), CostUSD: 0.5, Usage: core.TokenUsage{CacheRead: 7}, OK: true},          // hour 1
		{At: since.Add(3*time.Hour + 19*time.Minute), CostUSD: 0.25, OK: true, Duration: 2 * time.Second}, // hour 3
		{At: until, CostUSD: 100, OK: true},                                                               // after (exclusive end)
	} {
		if err := s.RecordRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RunsHourly(ctx, since, until)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		i            int
		runs, failed int64
		cost         float64
		tokens       int64
	}{{0, 2, 1, 3, 10}, {1, 1, 0, 0.5, 7}, {2, 0, 0, 0, 0}, {3, 1, 0, 0.25, 0}}
	if len(got) != len(tests) {
		t.Fatalf("hours: %d %+v", len(got), got)
	}
	var sum meter.RunStats
	for _, tc := range tests {
		h := got[tc.i]
		if h.Runs != tc.runs || h.Failed != tc.failed || h.CostUSD != tc.cost || h.Usage.Total() != tc.tokens {
			t.Errorf("hour %d: %+v", tc.i, h)
		}
		sum.Runs, sum.Failed, sum.CostUSD = sum.Runs+h.Runs, sum.Failed+h.Failed, sum.CostUSD+h.CostUSD
	}
	if got[3].DurationMS != 2000 {
		t.Errorf("duration %+v", got[3])
	}
	tot, err := s.RunTotals(ctx, meter.RunFilter{Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if tot.Runs != sum.Runs || tot.Failed != sum.Failed || tot.CostUSD != sum.CostUSD {
		t.Errorf("series %+v != totals %+v", sum, tot)
	}
	// Exactly 24 hours give 24 elements; an empty window gives none.
	if h, _ := s.RunsHourly(ctx, since, since.Add(24*time.Hour)); len(h) != 24 {
		t.Errorf("24h: %d", len(h))
	}
	if h, err := s.RunsHourly(ctx, since, since); h != nil || err != nil {
		t.Errorf("empty window: %v %v", h, err)
	}
	s.Close()
	if _, err := s.RunsHourly(ctx, since, until); err == nil {
		t.Error("closed store: no error")
	}
}

func TestDiagRunSummary(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	since := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	sum, err := s.DiagRunSummary(ctx, since)
	if err != nil || sum.Total != 0 || !sum.Last.IsZero() || sum.ByStatus == nil {
		t.Fatalf("empty index: %+v %v", sum, err)
	}
	rows := []struct {
		id     string
		at     time.Time
		status string
	}{
		{"20261001-090000-old", since.Add(-time.Hour), "failed"}, // before the window
		{"20261001-100000-a", since, "failed"},
		{"20261001-110000-b", since.Add(time.Hour), "interrupted"},
		{"20261001-120000-c", since.Add(2 * time.Hour), "postponed"},
		{"20261001-130000-d", since.Add(3 * time.Hour), "ok"},
		{"20261001-140000-e", since.Add(4 * time.Hour), "ok"},
	}
	for _, r := range rows {
		if err := s.SaveDiagRun(ctx, diagindex.Row{ID: r.id, StartedAt: r.at, Status: r.status, Kind: "review"}); err != nil {
			t.Fatal(err)
		}
	}
	sum, err = s.DiagRunSummary(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"failed": 1, "interrupted": 1, "postponed": 1, "ok": 2}
	if sum.Total != 5 || len(sum.ByStatus) != len(want) || !sum.Last.Equal(since.Add(4*time.Hour)) {
		t.Fatalf("summary %+v", sum)
	}
	for k, n := range want {
		if sum.ByStatus[k] != n {
			t.Errorf("%s: %d, want %d", k, sum.ByStatus[k], n)
		}
	}
	// Last ignores the window.
	if sum, _ := s.DiagRunSummary(ctx, since.Add(10*time.Hour)); sum.Total != 0 || !sum.Last.Equal(since.Add(4*time.Hour)) {
		t.Errorf("later window %+v", sum)
	}
	s.Close()
	if _, err := s.DiagRunSummary(ctx, since); err == nil {
		t.Error("closed store: no error")
	}
}
