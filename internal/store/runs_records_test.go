package store

import (
	"context"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/meter"
)

// The kind, MR and model conditions narrow every aggregate.
func TestRunFiltersKindMRModel(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	seedRuns(t, s, base)

	for _, tc := range []struct {
		name string
		f    meter.RunFilter
		runs int64
	}{
		{"kind", meter.RunFilter{Kind: "review"}, 2},
		{"kind none", meter.RunFilter{Kind: "map"}, 0},
		{"mr", meter.RunFilter{MRProjectPath: "g/api", MRIID: 7}, 2},
		{"mr other project", meter.RunFilter{MRProjectPath: "g/web", MRIID: 7}, 0},
		{"split model", meter.RunFilter{Model: "haiku", HasModel: true}, 1},
		{"requested model", meter.RunFilter{Model: "sonnet", HasModel: true}, 2},
		{"default model", meter.RunFilter{Model: "", HasModel: true}, 1},
		{"model and kind", meter.RunFilter{Model: "sonnet", HasModel: true, Kind: "chat"}, 1},
	} {
		tot, err := s.RunTotals(ctx, tc.f)
		if err != nil || tot.Runs != tc.runs {
			t.Errorf("%s: %+v %v", tc.name, tot, err)
		}
	}
	// A model filter keeps whole runs: the review with an opus+haiku split
	// counts all its tokens, and the by-model table still lists both.
	tot, _ := s.RunTotals(ctx, meter.RunFilter{Model: "haiku", HasModel: true})
	if tot.Usage.Total() != 135 {
		t.Fatalf("model filter keeps the whole run: %+v", tot)
	}
	models, _ := s.RunsBy(ctx, meter.RunFilter{Model: "haiku", HasModel: true}, meter.ByModel)
	if len(models) != 2 {
		t.Fatalf("by model under a model filter: %+v", models)
	}
	mrs, _ := s.TopMRs(ctx, meter.RunFilter{Kind: "review"}, 5)
	if len(mrs) != 2 {
		t.Fatalf("top mrs by kind: %+v", mrs)
	}
}

// RunRecords returns the runs newest first with their models, and Go's
// filter (RunFilter.Match) agrees with the SQL one: summing the records of
// a filter gives RunTotals of the same filter.
func TestRunRecordsMatchTotals(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	seedRuns(t, s, base)

	all, truncated, err := s.RunRecords(ctx, meter.RunFilter{}, 100)
	if err != nil || truncated || len(all) != 4 {
		t.Fatalf("records: %d %v %v", len(all), truncated, err)
	}
	if !all[0].At.Equal(base.Add(26*time.Hour)) || all[3].Kind != "review" || all[3].MRTitle != "feat: old title" ||
		len(all[3].Models) != 2 || all[1].OK || all[1].ErrorClass != "rate_limited" || all[3].DurationMS != 1000 || all[3].NumTurns != 3 {
		t.Fatalf("records: %+v", all)
	}
	bob, none := "bob", ""
	for _, f := range []meter.RunFilter{
		{}, {Username: &bob}, {Username: &none}, {ProjectPath: "g/api"}, {Kind: "review"},
		{MRProjectPath: "g/api", MRIID: 7}, {Model: "haiku", HasModel: true}, {Model: "", HasModel: true},
		{Model: "sonnet", HasModel: true, Username: &bob}, {Since: base.Add(time.Hour), Until: base.Add(26 * time.Hour)},
	} {
		tot, err := s.RunTotals(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		recs, _, err := s.RunRecords(ctx, f, 100)
		if err != nil {
			t.Fatal(err)
		}
		var runs, tokens int64
		var cost float64
		for _, r := range all {
			if f.Match(r) && (f.Since.IsZero() || !r.At.Before(f.Since)) && (f.Until.IsZero() || r.At.Before(f.Until)) {
				runs++
				tokens += r.Usage.Total()
				cost += r.CostUSD
			}
		}
		if int64(len(recs)) != tot.Runs || runs != tot.Runs || tokens != tot.Usage.Total() || cost != tot.CostUSD {
			t.Errorf("filter %+v: sql %+v, records %d, go %d/%d/%v", f, tot, len(recs), runs, tokens, cost)
		}
	}
	two, truncated, err := s.RunRecords(ctx, meter.RunFilter{}, 2)
	if err != nil || !truncated || len(two) != 2 || !two[0].At.Equal(base.Add(26*time.Hour)) {
		t.Fatalf("limit: %d %v %v", len(two), truncated, err)
	}
	if _, _, err := s.RunRecords(ctx, meter.RunFilter{}, 0); err == nil {
		t.Fatal("limit 0 accepted")
	}
}

func TestFirstRunAt(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if at, err := s.FirstRunAt(ctx); err != nil || !at.IsZero() {
		t.Fatalf("empty: %v %v", at, err)
	}
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	seedRuns(t, s, base)
	if at, err := s.FirstRunAt(ctx); err != nil || !at.Equal(base) {
		t.Fatalf("first: %v %v", at, err)
	}
}

func TestRunRecordsClosedDB(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if _, err := s.FirstRunAt(ctx); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, _, err := s.RunRecords(ctx, meter.RunFilter{}, 1); err == nil {
		t.Error("records on a closed db")
	}
	if _, err := s.FirstRunAt(ctx); err == nil {
		t.Error("first run on a closed db")
	}
}
