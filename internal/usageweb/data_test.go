package usageweb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
)

func decode(t *testing.T, h http.Handler, target string) Data {
	t.Helper()
	rec := get(h, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
	}
	var d Data
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// data.json carries the previous period, the chart buckets, the signals
// with their rules, the filter counts and the per-MR review count; no
// budget unless one is configured.
func TestDataAnalysis(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for i := 0; i < RereviewMin; i++ {
		if err := s.RecordRun(ctx, meter.ClaudeRun{At: now.Add(-time.Duration(30+i) * time.Minute), Kind: "review", ProjectPath: "group/api", MRIID: 7,
			Username: "alice", Model: "sonnet", Usage: core.TokenUsage{Input: 10}, CostUSD: 0.1, OK: true}); err != nil {
			t.Fatal(err)
		}
	}
	// previous 24 h: one run of alice
	if err := s.RecordRun(ctx, meter.ClaudeRun{At: now.Add(-30 * time.Hour), Kind: "chat", Username: "alice", Usage: core.TokenUsage{Input: 3}, CostUSD: 0.5, OK: true}); err != nil {
		t.Fatal(err)
	}
	h := newHandler(t, Options{Enabled: true, Store: s})
	raw := get(h, DataPath+"?period=24h", nil).Body.String()
	if strings.Contains(raw, `"budget"`) {
		t.Fatal("budget without usage.budget_usd")
	}
	d := decode(t, h, DataPath+"?period=24h")
	if d.Previous.Totals.Runs != 1 || d.Previous.Totals.CostUSD != 0.5 || d.Previous.Coverage != "full" {
		t.Fatalf("previous: %+v", d.Previous)
	}
	if d.Series.Step != Step30m || len(d.Series.Buckets) != 48 {
		t.Fatalf("series: %s %d", d.Series.Step, len(d.Series.Buckets))
	}
	var runs int64
	for _, b := range d.Series.Buckets {
		runs += b.Runs
	}
	if runs != d.Totals.Runs {
		t.Fatalf("buckets sum %d, totals %d", runs, d.Totals.Runs)
	}
	if signalByID(d.Signals, "rereview") == nil || signalByID(d.Signals, "nouser") == nil || d.Rules.RereviewMin != RereviewMin {
		t.Fatalf("signals: %+v", d.Signals)
	}
	if d.ReviewPerMR.MRs != 1 || len(d.Facets[FacetUser]) != 2 || len(d.Facets[FacetMR]) != 1 {
		t.Fatalf("review per mr / facets: %+v %+v", d.ReviewPerMR, d.Facets)
	}
	if d.TopMRs[0].Key != "group/api!7" || d.TopMRs[0].Reviews != RereviewMin+1 {
		t.Fatalf("top mrs: %+v", d.TopMRs)
	}
	for _, u := range d.ByUser {
		if u.PrevCostUSD == nil || (u.Key == "alice" && *u.PrevCostUSD != 0.5) {
			t.Fatalf("prev cost: %+v", u)
		}
	}
	// 30 days back reaches before the first run: no previous data at all.
	if d := decode(t, h, DataPath+"?period=90d"); d.Previous.Coverage != "none" || d.Series.Step != StepDay {
		t.Fatalf("90d: %+v", d.Previous)
	}
}

// The kind, model and mr filters narrow every part and are echoed.
func TestDataKindModelMRFilters(t *testing.T) {
	h := newHandler(t, Options{Enabled: true})
	d := decode(t, h, DataPath+"?period=7d&kind=review")
	if d.Totals.Runs != 1 || d.Filter.Kind != "review" || len(d.Facets[FacetKind]) != 3 {
		t.Fatalf("kind: %+v %+v", d.Totals, d.Facets[FacetKind])
	}
	d = decode(t, h, DataPath+"?period=7d&model=sonnet")
	if d.Totals.Runs != 1 || d.Filter.Model == nil || *d.Filter.Model != "sonnet" {
		t.Fatalf("model: %+v", d.Totals)
	}
	d = decode(t, h, DataPath+"?period=7d&model=-")
	if d.Totals.Runs != 2 || *d.Filter.Model != DefaultModel {
		t.Fatalf("default model: %+v", d.Totals)
	}
	d = decode(t, h, DataPath+"?period=7d&mr=group/api!7")
	if d.Totals.Runs != 1 || d.Filter.MR != "group/api!7" || len(d.TopMRs) != 1 {
		t.Fatalf("mr: %+v", d)
	}
	for _, bad := range []string{"?mr=group/api", "?mr=!7", "?mr=group/api!x", "?mr=group/api!0"} {
		if rec := get(h, DataPath+bad, nil); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bad mr") {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
}

func TestDataBudget(t *testing.T) {
	h := newHandler(t, Options{Enabled: true, BudgetUSD: 10})
	// the budget ignores the filters: month to date of every run
	d := decode(t, h, DataPath+"?period=24h&user=bob")
	b := d.Budget
	if b == nil || b.BudgetUSD != 10 || b.SpentUSD != 1.35 || b.MonthStart.Format("2006-01-02") != "2026-10-01" ||
		b.RatePerDayUSD != 1.35/7 || b.Rough || b.HistoryDays != 60 {
		t.Fatalf("budget: %+v", b)
	}
}

func TestDataKnownModel(t *testing.T) {
	s := newStore(t)
	if err := s.RecordRun(context.Background(), meter.ClaudeRun{At: now.Add(-time.Hour), Kind: "review", Model: "no-such", Username: "a", OK: false, ErrorClass: "failed"}); err != nil {
		t.Fatal(err)
	}
	h := newHandler(t, Options{Enabled: true, Store: s, KnownModel: func(m string) bool { return m == "sonnet" || m == "" }})
	f := signalByID(decode(t, h, DataPath+"?period=24h").Signals, "failed")
	if f == nil || f.Filter.Key != FacetModel || f.Filter.Value != "no-such" {
		t.Fatalf("failed: %+v", f)
	}
}

// failingStore fails one method.
type failingStore struct {
	Store
	fail string
}

var errBoom = errors.New("boom")

func (f failingStore) RunRecords(ctx context.Context, fl meter.RunFilter, n int) ([]meter.RunRecord, bool, error) {
	if f.fail == "records" {
		return nil, false, errBoom
	}
	return f.Store.RunRecords(ctx, fl, n)
}

func (f failingStore) FirstRunAt(ctx context.Context) (time.Time, error) {
	if f.fail == "first" {
		return time.Time{}, errBoom
	}
	return f.Store.FirstRunAt(ctx)
}

func (f failingStore) RunTotals(ctx context.Context, fl meter.RunFilter) (meter.RunStats, error) {
	// the page's totals pass; the budget's month and week queries fail
	if (f.fail == "month" && fl.Until.Equal(now) && fl.Since.Day() == 1 && fl.Username == nil && fl.Since.Hour() == 0) ||
		(f.fail == "week" && fl.Until.Equal(now) && fl.Since.Equal(now.Add(-7*24*time.Hour)) && fl.Location == nil) {
		return meter.RunStats{}, errBoom
	}
	return f.Store.RunTotals(ctx, fl)
}

func TestDataStoreErrors(t *testing.T) {
	for _, fail := range []string{"records", "first", "month", "week"} {
		h := newHandler(t, Options{Enabled: true, BudgetUSD: 5, Store: failingStore{Store: newStore(t), fail: fail}})
		if rec := get(h, DataPath+"?period=24h", nil); rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: %d", fail, rec.Code)
		}
	}
}
