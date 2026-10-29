package usageweb

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// rec is a successful run at t0+off.
func rec(off time.Duration, kind string, cost float64) meter.RunRecord {
	return meter.RunRecord{At: t0.Add(off), Kind: kind, CostUSD: cost, OK: true, Models: []string{"sonnet"}, Model: "sonnet",
		Usage: core.TokenUsage{Input: 10}}
}

func with(r meter.RunRecord, f func(*meter.RunRecord)) meter.RunRecord { f(&r); return r }

func TestStepFor(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour: Step15m, 6 * time.Hour: Step15m, 6*time.Hour + time.Minute: Step30m, 24 * time.Hour: Step30m,
		48 * time.Hour: Step30m, 49 * time.Hour: StepDay, 7 * 24 * time.Hour: StepDay, 90 * 24 * time.Hour: StepDay,
	} {
		if got := stepFor(d); got != want {
			t.Errorf("stepFor(%v) = %s, want %s", d, got, want)
		}
	}
}

func TestBucketStarts(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	since := time.Date(2026, 10, 20, 9, 7, 0, 0, time.UTC) // 12:07 MSK
	b := bucketStarts(since, since.Add(time.Hour), Step15m, msk)
	if len(b) != 5 || b[0].In(msk).Format("15:04") != "12:00" || b[4].In(msk).Format("15:04") != "13:00" {
		t.Fatalf("15m: %v", b)
	}
	b = bucketStarts(since, since.Add(24*time.Hour), Step30m, msk)
	if len(b) != 49 || b[0].In(msk).Format("15:04") != "12:00" || b[1].In(msk).Format("15:04") != "12:30" {
		t.Fatalf("30m: %d %v", len(b), b[:2])
	}
	b = bucketStarts(since, since.Add(7*24*time.Hour), StepDay, msk)
	if len(b) != 8 || b[0].In(msk).Format("2006-01-02 15:04") != "2026-10-20 00:00" {
		t.Fatalf("day: %v", b)
	}
	// Days are calendar days: across the DST switch one of them is 25 h.
	ber, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	s := time.Date(2026, 10, 24, 0, 0, 0, 0, ber)
	b = bucketStarts(s, s.AddDate(0, 0, 3), StepDay, ber)
	if len(b) != 3 || b[2].Sub(b[1]) != 25*time.Hour || b[2].In(ber).Hour() != 0 {
		t.Fatalf("dst: %v", b)
	}
	if got := bucketIndex(b, s.Add(-time.Second)); got != -1 {
		t.Errorf("before: %d", got)
	}
	if got := bucketIndex(b, b[2].Add(-time.Nanosecond)); got != 1 {
		t.Errorf("end of the long day: %d", got)
	}
	if len(bucketStarts(s, s.AddDate(5, 0, 0), StepDay, ber)) != 1000 {
		t.Error("cap")
	}
}

// The chart of periods of different lengths: the previous period lands at
// the same bucket index, runs outside both periods and other filters'
// runs are dropped, kinds and the repository split are summed.
func TestBuildSeries(t *testing.T) {
	alice := "alice"
	for _, tc := range []struct {
		name  string
		since time.Time
		until time.Time
		step  string
		n     int
	}{
		{"24h", t0.Add(-24 * time.Hour), t0, Step30m, 48},
		{"7d", t0.Add(-7 * 24 * time.Hour), t0, StepDay, 8},
		{"3h custom", t0.Add(-3 * time.Hour), t0, Step15m, 12},
	} {
		w := newWindow(tc.since, tc.until)
		recs := []meter.RunRecord{
			with(rec(-time.Minute, "review", 1), func(r *meter.RunRecord) { r.Username, r.ProjectPath = "alice", "g/a" }),
			with(rec(-2*time.Minute, "chat", 2), func(r *meter.RunRecord) { r.Username, r.ProjectPath = "alice", "g/b" }),
			with(rec(-3*time.Minute, "chat", 4), func(r *meter.RunRecord) { r.Username = "bob" }),
			with(rec(-tc.until.Sub(tc.since)-time.Minute, "review", 8), func(r *meter.RunRecord) { r.Username = "alice" }), // previous period, same slot
			rec(-3*tc.until.Sub(tc.since), "review", 16),                                                                   // outside both
			rec(time.Minute, "review", 32), // after until
		}
		s := buildSeries(recs, meter.RunFilter{Username: &alice}, w, time.UTC, []string{"g/a"})
		if s.Step != tc.step || len(s.Buckets) != tc.n {
			t.Fatalf("%s: step %s, %d buckets", tc.name, s.Step, len(s.Buckets))
		}
		last := s.Buckets[len(s.Buckets)-1]
		var runs int64
		var prev float64
		for _, b := range s.Buckets {
			runs += b.Runs
			prev += b.Prev.Cost
		}
		if runs != 2 || last.Runs != 2 || last.CostUSD != 3 || last.Kinds["chat"].Cost != 2 || last.Kinds["review"].Runs != 1 ||
			len(last.Split) != 1 || last.Split[0].Cost != 1 || prev != 8 || last.Prev.Cost != 8 || last.TotalTokens != 20 {
			t.Fatalf("%s: %+v (runs %d, prev %v)", tc.name, last, runs, prev)
		}
	}
	// Empty data: buckets with zeros.
	s := buildSeries(nil, meter.RunFilter{}, newWindow(t0.Add(-24*time.Hour), t0), time.UTC, nil)
	if len(s.Buckets) != 48 || s.Buckets[0].Runs != 0 || s.Buckets[0].Kinds != nil || s.Buckets[0].Split != nil {
		t.Fatalf("empty: %+v", s.Buckets[0])
	}
}

func TestCoverageAndPrevious(t *testing.T) {
	w := newWindow(t0.Add(-24*time.Hour), t0)
	for _, tc := range []struct {
		first time.Time
		want  string
	}{
		{time.Time{}, "none"}, {t0.Add(-time.Hour), "none"}, {w.since, "none"},
		{w.since.Add(-time.Hour), "partial"}, {w.prevSince, "full"}, {w.prevSince.Add(-time.Hour), "full"},
	} {
		if got := coverage(w, tc.first); got != tc.want {
			t.Errorf("first %v: %s, want %s", tc.first, got, tc.want)
		}
	}
	recs := []meter.RunRecord{
		with(rec(-30*time.Hour, "review", 1), func(r *meter.RunRecord) { r.MRIID, r.ProjectPath, r.NumTurns = 1, "g/a", 4 }),
		with(rec(-31*time.Hour, "verification", 0.5), func(r *meter.RunRecord) { r.MRIID, r.ProjectPath, r.NumTurns = 1, "g/a", 2 }),
		rec(-time.Hour, "review", 9),
	}
	first := t0.Add(-40 * time.Hour)
	p := previous(recs, meter.RunFilter{}, w, first)
	if p.Totals.Runs != 2 || p.Totals.CostUSD != 1.5 || p.Coverage != "partial" || p.HistoryFrom == nil || !p.HistoryFrom.Equal(first) ||
		p.ReviewPerMR.PerMRUSD != 1.5 || p.ReviewPerMR.MRs != 1 || p.AvgTurns != 3 || !p.Since.Equal(w.prevSince) || !p.Until.Equal(w.since) {
		t.Fatalf("previous: %+v", p)
	}
	if p := previous(nil, meter.RunFilter{}, w, time.Time{}); p.Coverage != "none" || p.HistoryFrom != nil || p.AvgTurns != 0 {
		t.Fatalf("empty previous: %+v", p)
	}
	if avgTurns(nil) != 0 || avgTurns(recs[:2]) != 3 {
		t.Error("avgTurns")
	}
	pc := prevCostByUser(append(recs, with(rec(-30*time.Hour, "chat", 2), func(r *meter.RunRecord) { r.Username = "bob" })), meter.RunFilter{}, w)
	if pc[UnattributedUser] != 1.5 || pc["bob"] != 2 || len(pc) != 2 {
		t.Fatalf("prev cost: %v", pc)
	}
}

func TestReviewCostAndMedian(t *testing.T) {
	for _, tc := range []struct {
		v    []float64
		want float64
	}{{nil, 0}, {[]float64{3}, 3}, {[]float64{3, 1, 2}, 2}, {[]float64{4, 1, 2, 3}, 2.5}, {[]float64{2, 2, 2, 2}, 2}} {
		if got := median(tc.v); got != tc.want {
			t.Errorf("median(%v) = %v", tc.v, got)
		}
	}
	if rc := reviewCost(nil); rc != (ReviewCost{}) {
		t.Fatalf("empty: %+v", rc)
	}
	mr := func(r meter.RunRecord, p string, iid int64) meter.RunRecord {
		r.ProjectPath, r.MRIID = p, iid
		return r
	}
	rc := reviewCost([]meter.RunRecord{
		mr(rec(0, "review", 1), "g/a", 1), mr(rec(0, "review", 3), "g/a", 1), mr(rec(0, "verification", 2), "g/a", 1),
		mr(rec(0, "review", 2), "g/b", 1),
		with(mr(rec(0, "review", 10), "g/b", 2), func(r *meter.RunRecord) { r.OK = false }), // failed: in per-MR, not in the median
		rec(0, "review", 100), // no MR
		mr(rec(0, "chat", 50), "g/a", 1),
	})
	if rc.MRs != 3 || rc.Runs != 5 || rc.PerMRUSD != 18.0/3 || rc.MedianRunUSD != 2 {
		t.Fatalf("review cost: %+v", rc)
	}
	if n := reviewsByMR([]meter.RunRecord{mr(rec(0, "review", 1), "g/a", 1), mr(rec(0, "review", 1), "g/a", 1), mr(rec(0, "chat", 1), "g/a", 1), rec(0, "review", 1)}); n["g/a!1"] != 2 || len(n) != 1 {
		t.Fatalf("reviews by mr: %v", n)
	}
}

func TestFacets(t *testing.T) {
	w := newWindow(t0.Add(-24*time.Hour), t0)
	recs := []meter.RunRecord{
		with(rec(-time.Hour, "review", 1), func(r *meter.RunRecord) {
			r.Username, r.ProjectPath, r.MRIID, r.MRTitle, r.Models = "alice", "g/a", 7, "новый заголовок", []string{"opus", "haiku"}
		}),
		with(rec(-2*time.Hour, "review", 1), func(r *meter.RunRecord) {
			r.Username, r.ProjectPath, r.MRIID, r.MRTitle = "alice", "g/a", 7, "старый"
		}),
		with(rec(-3*time.Hour, "chat", 1), func(r *meter.RunRecord) { r.Username, r.ProjectPath = "bob", "g/b" }),
		with(rec(-4*time.Hour, "map", 1), func(r *meter.RunRecord) { r.Models = []string{""} }),
		with(rec(-30*time.Hour, "chat", 1), func(r *meter.RunRecord) { r.Username = "carol" }), // previous period: not counted
	}
	alice := "alice"
	f := meter.RunFilter{Username: &alice}
	flt := Filter{User: &alice, Kind: "ci"}
	f.Kind = "ci" // nothing matches: the other fields count 0, the kind list still has every kind
	fc := facets(recs, f, flt, w)
	if len(fc[FacetUser]) != 1 || fc[FacetUser][0].Value != "alice" || fc[FacetUser][0].Count != 0 {
		t.Fatalf("selected user kept with 0: %+v", fc[FacetUser])
	}
	kinds := fc[FacetKind]
	if len(kinds) != 2 || kinds[0].Value != "review" || kinds[0].Count != 2 || kinds[0].Label != "Ревью" || kinds[1].Value != "ci" || kinds[1].Count != 0 {
		t.Fatalf("kinds: %+v", kinds)
	}
	fc = facets(recs, meter.RunFilter{}, Filter{}, w)
	users := map[string]Facet{}
	for _, x := range fc[FacetUser] {
		users[x.Value] = x
	}
	if len(users) != 3 || users[UnattributedUser].Label != "без пользователя" || users["alice"].Count != 2 || users["alice"].Label != "@alice" {
		t.Fatalf("users: %+v", fc[FacetUser])
	}
	if len(fc[FacetProject]) != 2 || fc[FacetProject][0].Value != "g/a" {
		t.Fatalf("projects (no empty path): %+v", fc[FacetProject])
	}
	models := map[string]Facet{}
	for _, x := range fc[FacetModel] {
		models[x.Value] = x
	}
	if models["opus"].Count != 1 || models["sonnet"].Count != 2 || models[DefaultModel].Label != "по умолчанию" {
		t.Fatalf("models: %+v", fc[FacetModel])
	}
	if m := fc[FacetMR]; len(m) != 1 || m[0].Value != "g/a!7" || m[0].Label != "g/a!7 новый заголовок" || m[0].Short != "a!7" || m[0].Count != 2 {
		t.Fatalf("mrs: %+v", m)
	}
	// Selected model and MR outside the data stay listed.
	none := DefaultModel
	fc = facets(recs, meter.RunFilter{Model: "x", HasModel: true}, Filter{Model: &none, MR: "g/z!1", Project: "g/z"}, w)
	if !hasFacet(fc[FacetModel], DefaultModel) || !hasFacet(fc[FacetMR], "g/z!1") || !hasFacet(fc[FacetProject], "g/z") {
		t.Fatalf("selected kept: %+v", fc)
	}
	// The MR list is capped, the selected MR survives the cap.
	var many []meter.RunRecord
	for i := 1; i <= maxMRFacets+5; i++ {
		r := rec(-time.Minute, "review", 1)
		r.ProjectPath, r.MRIID = "g/a", int64(i)
		many = append(many, r, r)
	}
	tail := rec(-time.Minute, "review", 1)
	tail.ProjectPath, tail.MRIID = "g/a", 9999
	many = append(many, tail)
	fc = facets(many, meter.RunFilter{}, Filter{MR: "g/a!9999"}, w)
	if len(fc[FacetMR]) != maxMRFacets || !hasFacet(fc[FacetMR], "g/a!9999") {
		t.Fatalf("cap: %d", len(fc[FacetMR]))
	}
	if without(meter.RunFilter{Kind: "x"}, "nope").Kind != "x" || selected(Filter{}, "nope") != "" {
		t.Error("unknown key")
	}
}

func signalByID(ss []Signal, id string) *Signal {
	for i := range ss {
		if ss[i].ID == id {
			return &ss[i]
		}
	}
	return nil
}

func TestSignalsEmptyAndQuiet(t *testing.T) {
	if s := signals(nil, nil); len(s) != 0 {
		t.Fatalf("empty data: %+v", s)
	}
	// Equal costs: no outlier; few reviews: no rereview; warm cache; all OK;
	// everyone attributed.
	var cur []meter.RunRecord
	for i := 0; i < 10; i++ {
		cur = append(cur, with(rec(0, "review", 1), func(r *meter.RunRecord) {
			r.Username, r.ProjectPath, r.MRIID = "alice", "g/a", int64(i%3+1)
			r.Usage = core.TokenUsage{Input: 1000, CacheRead: 90000}
		}))
	}
	if s := signals(cur, func(string) bool { return true }); len(s) != 0 {
		t.Fatalf("quiet: %+v", s)
	}
}

func TestRereviewSignal(t *testing.T) {
	var cur []meter.RunRecord
	add := func(p string, iid int64, n int, cost float64) {
		for i := 0; i < n; i++ {
			cur = append(cur, with(rec(time.Duration(i)*time.Minute, "review", cost), func(r *meter.RunRecord) { r.ProjectPath, r.MRIID = p, iid }))
		}
	}
	add("g/a", 1, RereviewMin-1, 1) // below the threshold
	if rereviewSignal(cur) != nil {
		t.Fatal("below threshold")
	}
	add("g/b", 2, RereviewMin, 2)
	add("g/c", 3, RereviewMin+3, 1)
	add("g/d", 4, RereviewMin, 1)
	add("g/e", 5, RereviewMin, 1)
	s := rereviewSignal(cur)
	if s == nil || s.Filter.Key != FacetMR || s.Filter.Value != "g/c!3" || s.Filter.Label != "MR c!3" || s.Runs != 4*RereviewMin+3 ||
		s.Cost != float64(2*(RereviewMin-1)+(RereviewMin+2)+2*(RereviewMin-1)) || !strings.HasPrefix(s.Detail, "c!3 ×8, b!2 ×5, d!4 ×5, и ещё 1 MR — от 5 ревью") {
		t.Fatalf("rereview: %+v", s)
	}
}

func TestOutlierSignal(t *testing.T) {
	base := func(n int, cost float64) []meter.RunRecord {
		var out []meter.RunRecord
		for i := 0; i < n; i++ {
			out = append(out, rec(0, "review", cost))
		}
		return out
	}
	for _, tc := range []struct {
		name string
		cur  []meter.RunRecord
		want int
	}{
		{"tiny sample", append(base(OutlierMinSamples-2, 0.1), rec(0, "review", 5)), 0}, // 4 successful runs incl. the big one
		{"equal medians", base(10, 0.2), 0},
		{"below the absolute floor", append(base(10, 0.001), rec(0, "review", 0.04)), 0},
		{"zero median", append(base(10, 0), rec(0, "review", 1)), 0},
		{"exactly 3x", append(base(10, 0.1), rec(0, "review", 0.3)), 0},
		{"outlier", append(base(10, 0.1), rec(0, "review", 0.5), with(rec(0, "review", 0.9), func(r *meter.RunRecord) { r.ProjectPath, r.MRIID = "g/a", 4 })), 2},
		{"failed outlier counts, failed runs not in the median", append(base(5, 0.1), with(rec(0, "review", 9), func(r *meter.RunRecord) { r.OK = false })), 1},
	} {
		s := outlierSignal(tc.cur)
		got := 0
		if s != nil {
			got = s.Runs
		}
		if got != tc.want {
			t.Errorf("%s: %d outliers, want %d (%+v)", tc.name, got, tc.want, s)
		}
	}
	s := outlierSignal(append(base(10, 0.1), rec(0, "review", 0.5), with(rec(0, "review", 0.9), func(r *meter.RunRecord) { r.ProjectPath, r.MRIID = "g/a", 4 })))
	if s.Filter.Key != FacetMR || s.Filter.Value != "g/a!4" || math.Abs(s.Cost-1.4) > 1e-9 || !strings.Contains(s.Detail, "Ревью a!4 — $0.90 при медиане $0.10; Ревью — $0.50") {
		t.Fatalf("outlier: %+v", s)
	}
	s = outlierSignal(append(base(10, 0.1), with(rec(0, "chat", 0.1), func(r *meter.RunRecord) { r.ProjectPath = "g/b" }), rec(0, "review", 0.5)))
	if s.Filter.Key != FacetKind || s.Filter.Value != "review" || s.Runs != 1 {
		t.Fatalf("outlier without MR: %+v", s)
	}
	s = outlierSignal(append(base(10, 0.1), with(rec(0, "review", 0.5), func(r *meter.RunRecord) { r.ProjectPath = "g/b" })))
	if !strings.Contains(s.Detail, "Ревью b — $0.50") {
		t.Fatalf("outlier in a repository: %s", s.Detail)
	}
}

func TestColdSignal(t *testing.T) {
	cold := func(p string, in, cr int64, cost float64) meter.RunRecord {
		return with(rec(0, "review", cost), func(r *meter.RunRecord) { r.ProjectPath, r.Usage = p, core.TokenUsage{Input: in, CacheRead: cr} })
	}
	if coldSignal([]meter.RunRecord{cold("g/a", ColdMinInput-1, 0, 1), cold("g/a", 1000, ColdMinInput, 1)}) != nil {
		t.Fatal("small input or warm cache flagged")
	}
	s := coldSignal([]meter.RunRecord{cold("g/a", ColdMinInput, 0, 1), cold("g/b", 30000, 1000, 2), cold("g/b", 30000, 1000, 2), cold("", 50000, 0, 9)})
	if s == nil || s.Runs != 4 || s.Cost != 14 || s.Filter == nil || s.Filter.Value != "g/b" || !strings.Contains(s.Detail, "меньше 10% входа") {
		t.Fatalf("cold: %+v", s)
	}
	s = coldSignal([]meter.RunRecord{cold("", 50000, 0, 9)})
	if s == nil || s.Filter != nil {
		t.Fatalf("cold without a repository: %+v", s)
	}
}

func TestFailedSignal(t *testing.T) {
	fail := func(kind, model, class string) meter.RunRecord {
		return with(rec(0, kind, 0.01), func(r *meter.RunRecord) { r.OK, r.Model, r.ErrorClass = false, model, class })
	}
	known := func(m string) bool { return strings.Contains(m, "sonnet") }
	cur := []meter.RunRecord{rec(0, "review", 1), fail("review", "sonnet", "timeout"), fail("chat", "no-such-model", ""), fail("chat", "no-such-model", "failed"), fail("chat", "gpt", "weird")}
	s := failedSignal(cur, known)
	if s == nil || s.Runs != 4 || s.Title != "4 запуска с ошибкой" || s.Filter.Key != FacetModel || s.Filter.Value != "gpt" ||
		!strings.Contains(s.Detail, "ошибка ×2, таймаут ×1, weird ×1.") || !strings.Contains(s.Detail, "«gpt», «no-such-model»") {
		t.Fatalf("failed: %+v", s)
	}
	s = failedSignal(cur, nil)
	if s.Filter.Key != FacetKind || s.Filter.Value != "chat" || strings.Contains(s.Detail, "Модель") {
		t.Fatalf("failed without a model check: %+v", s)
	}
	if failedSignal(cur[:1], known) != nil {
		t.Fatal("no failures")
	}
	if errorClassLabel("rate_limited") != "лимит запросов" {
		t.Error("label")
	}
}

func TestNoUserSignal(t *testing.T) {
	u := func(name string, cost float64) meter.RunRecord {
		return with(rec(0, "map", cost), func(r *meter.RunRecord) { r.Username = name })
	}
	if noUserSignal([]meter.RunRecord{u("a", 1)}) != nil || noUserSignal([]meter.RunRecord{u("", 1), u("", 2)}) != nil {
		t.Fatal("none or everyone unattributed")
	}
	s := noUserSignal([]meter.RunRecord{u("", 1), u("a", 3)})
	if s == nil || s.CostNote != "25% стоимости" || s.Filter.Value != UnattributedUser || s.Runs != 1 {
		t.Fatalf("nouser: %+v", s)
	}
	if s := noUserSignal([]meter.RunRecord{u("", 1), u("a", 2)}); s.CostNote != "33,3% стоимости" {
		t.Fatalf("share: %q", s.CostNote)
	}
	if s := noUserSignal([]meter.RunRecord{u("", 0), u("a", 0)}); s.CostNote != "0% стоимости" {
		t.Fatalf("zero: %q", s.CostNote)
	}
}

func TestSignalsOrder(t *testing.T) {
	var cur []meter.RunRecord
	for i := 0; i < RereviewMin; i++ {
		cur = append(cur, with(rec(0, "review", 0.1), func(r *meter.RunRecord) { r.ProjectPath, r.MRIID, r.Username = "g/a", 1, "a" }))
	}
	cur = append(cur, with(rec(0, "review", 2), func(r *meter.RunRecord) {
		r.ProjectPath, r.Usage, r.OK = "g/a", core.TokenUsage{Input: 50000}, false
	}))
	var ids []string
	for _, s := range signals(cur, nil) {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "rereview,outlier,cold,failed,nouser" {
		t.Fatalf("order: %v", ids)
	}
}

func TestBudget(t *testing.T) {
	msk := time.FixedZone("MSK", 3*3600)
	for _, tc := range []struct {
		name     string
		now      time.Time
		start    string
		daysLeft float64
	}{
		{"mid month", time.Date(2026, 10, 16, 0, 0, 0, 0, msk), "2026-10-01", 16},
		{"first moment of the month", time.Date(2026, 11, 1, 0, 0, 0, 0, msk), "2026-11-01", 30},
		{"last minute of the month", time.Date(2026, 10, 31, 23, 59, 0, 0, msk), "2026-10-01", 1.0 / 24 / 60},
		{"utc instant in the next local month", time.Date(2026, 10, 31, 22, 0, 0, 0, time.UTC), "2026-11-01", 30 - 1.0/24},
	} {
		b := budgetOf(100, 20, 14, tc.now, msk, tc.now.Add(-30*24*time.Hour))
		if b.MonthStart.Format("2006-01-02") != tc.start || math.Abs(b.DaysLeft-tc.daysLeft) > 1e-9 || b.RatePerDayUSD != 2 ||
			math.Abs(b.ForecastUSD-(20+2*tc.daysLeft)) > 1e-9 || b.SpentShare != 0.2 || b.Rough || b.MonthEnd.Sub(b.MonthStart) < 28*24*time.Hour {
			t.Errorf("%s: %+v", tc.name, b)
		}
	}
	b := budgetOf(50, 0, 0, t0, time.UTC, time.Time{})
	if !b.Rough || b.HistoryDays != 0 || b.ForecastUSD != 0 {
		t.Fatalf("no history: %+v", b)
	}
	if b := budgetOf(50, 5, 7, t0, time.UTC, t0.Add(-48*time.Hour)); !b.Rough || b.HistoryDays != 2 {
		t.Fatalf("short history: %+v", b)
	}
}

func TestFormatting(t *testing.T) {
	for v, want := range map[float64]string{0: "$0.000", 0.004: "<$0.01", 0.05: "$0.050", 0.1: "$0.10", 12.345: "$12.35"} {
		if got := usd(v); got != want {
			t.Errorf("usd(%v) = %q, want %q", v, got, want)
		}
	}
	for n, want := range map[int]string{1: "запуск", 2: "запуска", 5: "запусков", 11: "запусков", 21: "запуск", 22: "запуска", 112: "запусков"} {
		if got := plural(n, "запуск", "запуска", "запусков"); got != want {
			t.Errorf("plural(%d) = %s", n, got)
		}
	}
	if topKey(map[string]float64{"": 9, "b": 1, "a": 1}) != "a" || topKey(nil) != "" {
		t.Error("topKey")
	}
	if repoName("g/sub/x") != "x" || repoName("x") != "x" || userLabel("-") != "без пользователя" {
		t.Error("labels")
	}
}
