package usageweb

// Per-run rules of the usage page, computed in Go over the run records
// (meter.RunRecord) so the browser never gets the raw runs: time buckets
// with the previous period, the previous period's totals, the filter
// counts, the cost of one MR review, the "На что обратить внимание"
// signals and the monthly budget. Everything here is a pure function of
// its arguments (table-tested in analysis_test.go).

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/meter"
)

// Signal thresholds. The page states them next to the signals (Rules).
const (
	// RereviewMin: an MR with at least this many review runs in the period
	// is "reviewed again and again".
	RereviewMin = 5
	// OutlierFactor, OutlierMinSamples, OutlierMinUSD: a run costs more than
	// OutlierFactor × the median of the successful runs of its kind; the
	// kind needs OutlierMinSamples successful runs for a median, and runs
	// cheaper than OutlierMinUSD are never outliers (noise).
	OutlierFactor     = 3
	OutlierMinSamples = 5
	OutlierMinUSD     = 0.05
	// ColdMinInput, ColdMaxShare: a run with at least ColdMinInput input
	// tokens (input + cache read + cache write) of which less than
	// ColdMaxShare were read from the prompt cache.
	ColdMinInput = 20000
	ColdMaxShare = 0.10
)

// Rules echoes the thresholds for the page's explanation.
type Rules struct {
	RereviewMin       int     `json:"rereview_min"`
	OutlierFactor     int     `json:"outlier_factor"`
	OutlierMinSamples int     `json:"outlier_min_samples"`
	OutlierMinUSD     float64 `json:"outlier_min_usd"`
	ColdMinInput      int     `json:"cold_min_input"`
	ColdMaxShare      float64 `json:"cold_max_share"`
}

var rules = Rules{RereviewMin, OutlierFactor, OutlierMinSamples, OutlierMinUSD, ColdMinInput, ColdMaxShare}

// ---------------------------------------------------------------------------
// windows and buckets
// ---------------------------------------------------------------------------

// Bucket steps of the chart.
const (
	Step15m = "15m"
	Step30m = "30m"
	StepDay = "day"
)

// stepFor is the chart step of a period of length d: 15 minutes up to 6
// hours, 30 minutes up to 2 days, a calendar day beyond.
func stepFor(d time.Duration) string {
	switch {
	case d <= 6*time.Hour:
		return Step15m
	case d <= 48*time.Hour:
		return Step30m
	}
	return StepDay
}

// bucketStarts returns the bucket starts covering [since, until): step-
// aligned in loc for 15m/30m, local midnights for days (DST-safe: days are
// calendar days). At most maxBuckets.
func bucketStarts(since, until time.Time, step string, loc *time.Location) []time.Time {
	const maxBuckets = 1000
	s := since.In(loc)
	var out []time.Time
	if step == StepDay {
		for d := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, loc); d.Before(until) && len(out) < maxBuckets; d = d.AddDate(0, 0, 1) {
			out = append(out, d)
		}
		return out
	}
	m := 15
	if step == Step30m {
		m = 30
	}
	mid := time.Date(s.Year(), s.Month(), s.Day(), s.Hour(), s.Minute()/m*m, 0, 0, loc)
	for t := mid; t.Before(until) && len(out) < maxBuckets; t = t.Add(time.Duration(m) * time.Minute) {
		out = append(out, t)
	}
	return out
}

// bucketIndex is the index of the bucket of t among starts (sorted), -1
// before the first one.
func bucketIndex(starts []time.Time, t time.Time) int {
	return sort.Search(len(starts), func(i int) bool { return starts[i].After(t) }) - 1
}

// Part is a slice of a bucket: runs, tokens, cost.
type Part struct {
	Runs   int64   `json:"runs"`
	Tokens int64   `json:"total_tokens"`
	Cost   float64 `json:"cost_usd"`
}

func (p *Part) add(r meter.RunRecord) {
	p.Runs++
	p.Tokens += r.Usage.Total()
	p.Cost += r.CostUSD
}

// Bucket is one interval of the chart: the current period's sums, split
// by run kind (and by repository when Series.SplitKeys is set), and the
// previous period's sums at the same position.
type Bucket struct {
	Start time.Time `json:"start"`
	Stats
	Kinds map[string]Part `json:"kinds,omitempty"`
	Split []Part          `json:"split,omitempty"`
	Prev  Part            `json:"prev"`
}

// Series is the chart data.
type Series struct {
	Step string `json:"step"`
	// SplitKeys, when set, are the repositories Bucket.Split is aligned
	// with (a user filter without a repository filter: the user's chart is
	// split by repository; the rest is total minus the split).
	SplitKeys []string `json:"split_keys,omitempty"`
	Buckets   []Bucket `json:"buckets"`
}

// window is a period with its previous period of equal length.
type window struct {
	since, until, prevSince time.Time
}

func (w window) inCur(t time.Time) bool  { return !t.Before(w.since) && t.Before(w.until) }
func (w window) inPrev(t time.Time) bool { return !t.Before(w.prevSince) && t.Before(w.since) }

func newWindow(since, until time.Time) window {
	return window{since: since, until: until, prevSince: since.Add(-until.Sub(since))}
}

// buildSeries buckets the records matching f. The previous period is
// aligned by bucket index: its buckets are built from prevSince the same
// way, so day buckets stay calendar days across DST.
func buildSeries(recs []meter.RunRecord, f meter.RunFilter, w window, loc *time.Location, splitKeys []string) Series {
	step := stepFor(w.until.Sub(w.since))
	cur := bucketStarts(w.since, w.until, step, loc)
	prev := bucketStarts(w.prevSince, w.since, step, loc)
	s := Series{Step: step, SplitKeys: splitKeys, Buckets: make([]Bucket, len(cur))}
	acc := make([]meter.RunStats, len(cur))
	split := map[string]int{}
	for i, k := range splitKeys {
		split[k] = i
	}
	for i, t := range cur {
		s.Buckets[i].Start = t
		if len(splitKeys) > 0 {
			s.Buckets[i].Split = make([]Part, len(splitKeys))
		}
	}
	for _, r := range recs {
		if !f.Match(r) {
			continue
		}
		switch {
		case w.inCur(r.At):
			i := bucketIndex(cur, r.At) // >= 0: the first bucket starts at or before since
			addStats(&acc[i], r)
			b := &s.Buckets[i]
			if b.Kinds == nil {
				b.Kinds = map[string]Part{}
			}
			p := b.Kinds[r.Kind]
			p.add(r)
			b.Kinds[r.Kind] = p
			if j, ok := split[r.ProjectPath]; ok {
				b.Split[j].add(r)
			}
		case w.inPrev(r.At):
			if i := bucketIndex(prev, r.At); i >= 0 && i < len(cur) {
				s.Buckets[i].Prev.add(r)
			}
		}
	}
	for i := range s.Buckets {
		s.Buckets[i].Stats = toStats(acc[i])
	}
	return s
}

func addStats(s *meter.RunStats, r meter.RunRecord) {
	s.Runs++
	if !r.OK {
		s.Failed++
	}
	s.Usage.Input += r.Usage.Input
	s.Usage.Output += r.Usage.Output
	s.Usage.CacheRead += r.Usage.CacheRead
	s.Usage.CacheWrite += r.Usage.CacheWrite
	s.CostUSD += r.CostUSD
	s.DurationMS += r.DurationMS
}

// ---------------------------------------------------------------------------
// previous period
// ---------------------------------------------------------------------------

// Previous is the previous period of equal length.
type Previous struct {
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
	Totals Stats     `json:"totals"`
	// Coverage: "full" — runs are stored for the whole previous period;
	// "partial" — the history starts inside it (HistoryFrom); "none" — it
	// starts after it, so the previous period has no data (not "zero").
	Coverage    string     `json:"coverage"`
	HistoryFrom *time.Time `json:"history_from,omitempty"`
	ReviewPerMR ReviewCost `json:"review_per_mr"`
	// AvgTurns is the average number of turns per run.
	AvgTurns float64 `json:"avg_turns"`
}

// coverage of the previous period given the oldest stored run (zero = none).
func coverage(w window, first time.Time) string {
	switch {
	case first.IsZero() || !first.Before(w.since):
		return "none"
	case first.After(w.prevSince):
		return "partial"
	}
	return "full"
}

func previous(recs []meter.RunRecord, f meter.RunFilter, w window, first time.Time) Previous {
	var st meter.RunStats
	var in []meter.RunRecord
	var turns int64
	for _, r := range recs {
		if f.Match(r) && w.inPrev(r.At) {
			addStats(&st, r)
			turns += int64(r.NumTurns)
			in = append(in, r)
		}
	}
	p := Previous{Since: w.prevSince, Until: w.since, Totals: toStats(st), Coverage: coverage(w, first), ReviewPerMR: reviewCost(in)}
	if p.Coverage == "partial" {
		p.HistoryFrom = &first
	}
	if st.Runs > 0 {
		p.AvgTurns = float64(turns) / float64(st.Runs)
	}
	return p
}

// current returns the records of the current period matching f.
func current(recs []meter.RunRecord, f meter.RunFilter, w window) []meter.RunRecord {
	var out []meter.RunRecord
	for _, r := range recs {
		if f.Match(r) && w.inCur(r.At) {
			out = append(out, r)
		}
	}
	return out
}

// avgTurns is the average number of turns of recs.
func avgTurns(recs []meter.RunRecord) float64 {
	if len(recs) == 0 {
		return 0
	}
	var n int64
	for _, r := range recs {
		n += int64(r.NumTurns)
	}
	return float64(n) / float64(len(recs))
}

// prevCostByUser sums the previous period's cost per user ("-" for
// unattributed) for the "Δ стоимости" column.
func prevCostByUser(recs []meter.RunRecord, f meter.RunFilter, w window) map[string]float64 {
	out := map[string]float64{}
	for _, r := range recs {
		if f.Match(r) && w.inPrev(r.At) {
			out[userKey(r.Username)] += r.CostUSD
		}
	}
	return out
}

func userKey(u string) string {
	if u == "" {
		return UnattributedUser
	}
	return u
}

func modelKey(m string) string {
	if m == "" {
		return DefaultModel
	}
	return m
}

func mrKey(project string, iid int64) string { return project + "!" + strconv.FormatInt(iid, 10) }

// ---------------------------------------------------------------------------
// review economics
// ---------------------------------------------------------------------------

// ReviewCost is what reviewing one MR costs: review + verification runs
// attached to an MR, averaged per MR.
type ReviewCost struct {
	PerMRUSD float64 `json:"per_mr_usd"`
	MRs      int     `json:"mrs"`
	Runs     int     `json:"runs"`
	// MedianRunUSD is the median cost of one successful review run.
	MedianRunUSD float64 `json:"median_run_usd"`
}

func reviewCost(recs []meter.RunRecord) ReviewCost {
	var rc ReviewCost
	mrs := map[string]bool{}
	var cost float64
	var reviews []float64
	for _, r := range recs {
		if r.MRIID <= 0 || (r.Kind != meter.KindReview && r.Kind != meter.KindVerification) {
			continue
		}
		mrs[mrKey(r.ProjectPath, r.MRIID)] = true
		cost += r.CostUSD
		rc.Runs++
		if r.Kind == meter.KindReview && r.OK {
			reviews = append(reviews, r.CostUSD)
		}
	}
	rc.MRs = len(mrs)
	if rc.MRs > 0 {
		rc.PerMRUSD = cost / float64(rc.MRs)
	}
	rc.MedianRunUSD = median(reviews)
	return rc
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	h := len(s) / 2
	if len(s)%2 == 1 {
		return s[h]
	}
	return (s[h-1] + s[h]) / 2
}

// reviewsByMR counts the review runs per MR key (the "Ревью" column of the
// MR table).
func reviewsByMR(recs []meter.RunRecord) map[string]int64 {
	out := map[string]int64{}
	for _, r := range recs {
		if r.MRIID > 0 && r.Kind == meter.KindReview {
			out[mrKey(r.ProjectPath, r.MRIID)]++
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// filter counts
// ---------------------------------------------------------------------------

// Facet is one option of a filter field: the runs it would show with the
// other filters kept.
type Facet struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Short string `json:"short,omitempty"`
	Count int64  `json:"count"`
}

// maxMRFacets bounds the MR list of the filter bar.
const maxMRFacets = 300

// Filter field keys (data.json facets, URL parameters).
const (
	FacetUser    = "user"
	FacetProject = "project"
	FacetKind    = "kind"
	FacetModel   = "model"
	FacetMR      = "mr"
)

// without returns f without the condition of one filter field.
func without(f meter.RunFilter, key string) meter.RunFilter {
	switch key {
	case FacetUser:
		f.Username = nil
	case FacetProject:
		f.ProjectPath = ""
	case FacetKind:
		f.Kind = ""
	case FacetModel:
		f.Model, f.HasModel = "", false
	case FacetMR:
		f.MRProjectPath, f.MRIID = "", 0
	}
	return f
}

// facets counts, for every filter field, the runs of the current period
// per value with the other filters applied. The selected value stays in
// its list (count 0 when the other filters hide it).
func facets(recs []meter.RunRecord, f meter.RunFilter, flt Filter, w window) map[string][]Facet {
	out := map[string][]Facet{}
	for _, key := range []string{FacetUser, FacetProject, FacetKind, FacetModel, FacetMR} {
		g := without(f, key)
		count := map[string]int64{}
		labels := map[string][2]string{}
		for _, r := range recs {
			if !w.inCur(r.At) || !g.Match(r) {
				continue
			}
			switch key {
			case FacetUser:
				count[userKey(r.Username)]++
			case FacetProject:
				if r.ProjectPath != "" {
					count[r.ProjectPath]++
				}
			case FacetKind:
				count[r.Kind]++
			case FacetModel:
				for _, m := range r.Models {
					count[modelKey(m)]++
				}
			case FacetMR:
				if r.MRIID > 0 {
					k := mrKey(r.ProjectPath, r.MRIID)
					count[k]++
					if l := labels[k]; l[0] == "" && r.MRTitle != "" {
						// records come newest first: the first title is the latest
						labels[k] = [2]string{r.MRTitle}
					}
				}
			}
		}
		if sel := selected(flt, key); sel != "" {
			if _, ok := count[sel]; !ok {
				count[sel] = 0
			}
		}
		list := make([]Facet, 0, len(count))
		for v, n := range count {
			list = append(list, facetOf(key, v, n, labels[v][0]))
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Count != list[j].Count {
				return list[i].Count > list[j].Count
			}
			return list[i].Value < list[j].Value
		})
		if key == FacetMR && len(list) > maxMRFacets {
			keep := list[:maxMRFacets]
			if sel := selected(flt, key); sel != "" && !hasFacet(keep, sel) {
				keep = append(keep[:maxMRFacets-1], facetOf(key, sel, count[sel], labels[sel][0]))
			}
			list = keep
		}
		out[key] = list
	}
	return out
}

func hasFacet(list []Facet, v string) bool {
	for _, x := range list {
		if x.Value == v {
			return true
		}
	}
	return false
}

func selected(flt Filter, key string) string {
	switch key {
	case FacetUser:
		if flt.User != nil {
			return userKey(*flt.User)
		}
	case FacetProject:
		return flt.Project
	case FacetKind:
		return flt.Kind
	case FacetModel:
		if flt.Model != nil {
			return modelKey(*flt.Model)
		}
	case FacetMR:
		return flt.MR
	}
	return ""
}

func facetOf(key, v string, n int64, title string) Facet {
	f := Facet{Value: v, Label: v, Count: n}
	switch key {
	case FacetUser:
		f.Label = userLabel(v)
	case FacetKind:
		f.Label = label(meter.ByKind, v)
	case FacetModel:
		if v == DefaultModel {
			f.Label = label(meter.ByModel, "")
		}
	case FacetMR:
		p, iid, _ := strings.Cut(v, "!")
		f.Short = repoName(p) + "!" + iid
		if title != "" {
			f.Label = v + " " + title
		}
	}
	return f
}

func userLabel(u string) string {
	if u == UnattributedUser {
		return "без пользователя"
	}
	return "@" + u
}

func repoName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ---------------------------------------------------------------------------
// signals
// ---------------------------------------------------------------------------

// Signal is one line of "На что обратить внимание".
type Signal struct {
	ID string `json:"id"` // rereview | outlier | cold | failed | nouser
	// Level: warn, bad or info.
	Level  string  `json:"level"`
	Title  string  `json:"title"`
	Detail string  `json:"detail"`
	Runs   int     `json:"runs"`
	Cost   float64 `json:"cost_usd"`
	// CostNote says what Cost is ("повторы после первого", …).
	CostNote string `json:"cost_note"`
	// Filter is what a click on the signal sets.
	Filter *SignalFilter `json:"filter,omitempty"`
}

// SignalFilter is a filter field and value (a Facet key and value).
type SignalFilter struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Label string `json:"label"`
}

func runsWord(n int) string {
	return strconv.Itoa(n) + " " + plural(n, "запуск", "запуска", "запусков")
}

func plural(n int, one, few, many string) string {
	m10, m100 := n%10, n%100
	switch {
	case m10 == 1 && m100 != 11:
		return one
	case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
		return few
	}
	return many
}

func usd(v float64) string {
	if v != 0 && v < 0.01 {
		return "<$0.01"
	}
	if v < 0.1 {
		return fmt.Sprintf("$%.3f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func mrRef(project string, iid int64) string {
	return repoName(project) + "!" + strconv.FormatInt(iid, 10)
}

// signals applies the rules to the current period's runs (already
// filtered). knownModel reports whether a requested model name is a
// Claude model (nil: every name is).
func signals(cur []meter.RunRecord, knownModel func(string) bool) []Signal {
	var out []Signal
	if s := rereviewSignal(cur); s != nil {
		out = append(out, *s)
	}
	if s := outlierSignal(cur); s != nil {
		out = append(out, *s)
	}
	if s := coldSignal(cur); s != nil {
		out = append(out, *s)
	}
	if s := failedSignal(cur, knownModel); s != nil {
		out = append(out, *s)
	}
	if s := noUserSignal(cur); s != nil {
		out = append(out, *s)
	}
	return out
}

type mrGroup struct {
	key, project string
	iid          int64
	runs         []meter.RunRecord
}

func rereviewSignal(cur []meter.RunRecord) *Signal {
	byMR := map[string]*mrGroup{}
	for _, r := range cur {
		if r.Kind != meter.KindReview || r.MRIID <= 0 {
			continue
		}
		k := mrKey(r.ProjectPath, r.MRIID)
		g := byMR[k]
		if g == nil {
			g = &mrGroup{key: k, project: r.ProjectPath, iid: r.MRIID}
			byMR[k] = g
		}
		g.runs = append(g.runs, r)
	}
	var hot []*mrGroup
	for _, g := range byMR {
		if len(g.runs) >= RereviewMin {
			hot = append(hot, g)
		}
	}
	if len(hot) == 0 {
		return nil
	}
	sort.Slice(hot, func(i, j int) bool {
		if len(hot[i].runs) != len(hot[j].runs) {
			return len(hot[i].runs) > len(hot[j].runs)
		}
		return hot[i].key < hot[j].key
	})
	var extra float64
	runs := 0
	for _, g := range hot {
		sort.Slice(g.runs, func(i, j int) bool { return g.runs[i].At.Before(g.runs[j].At) })
		for _, r := range g.runs[1:] {
			extra += r.CostUSD
		}
		runs += len(g.runs)
	}
	var names []string
	for i, g := range hot {
		if i == 3 {
			names = append(names, fmt.Sprintf("и ещё %d MR", len(hot)-3))
			break
		}
		names = append(names, fmt.Sprintf("%s ×%d", mrRef(g.project, g.iid), len(g.runs)))
	}
	top := hot[0]
	return &Signal{ID: "rereview", Level: "warn", Title: "Повторные ревью одного MR",
		Detail: strings.Join(names, ", ") + fmt.Sprintf(" — от %d ревью одного MR за период: каждый пуш запускает ревью заново.", RereviewMin),
		Runs:   runs, Cost: extra, CostNote: "повторы после первого",
		Filter: &SignalFilter{Key: FacetMR, Value: top.key, Label: "MR " + mrRef(top.project, top.iid)}}
}

func outlierSignal(cur []meter.RunRecord) *Signal {
	costs := map[string][]float64{}
	for _, r := range cur {
		if r.OK {
			costs[r.Kind] = append(costs[r.Kind], r.CostUSD)
		}
	}
	med := map[string]float64{}
	for k, v := range costs {
		if len(v) >= OutlierMinSamples {
			med[k] = median(v)
		}
	}
	var outl []meter.RunRecord
	var cost float64
	for _, r := range cur {
		m := med[r.Kind]
		if m > 0 && r.CostUSD >= OutlierMinUSD && r.CostUSD > OutlierFactor*m {
			outl = append(outl, r)
			cost += r.CostUSD
		}
	}
	if len(outl) == 0 {
		return nil
	}
	sort.SliceStable(outl, func(i, j int) bool { return outl[i].CostUSD > outl[j].CostUSD })
	var parts []string
	for i, r := range outl {
		if i == 2 {
			break
		}
		where := r.ProjectPath
		if r.MRIID > 0 {
			where = mrRef(r.ProjectPath, r.MRIID)
		} else if where != "" {
			where = repoName(where)
		}
		parts = append(parts, strings.TrimSpace(label(meter.ByKind, r.Kind)+" "+where)+" — "+usd(r.CostUSD)+" при медиане "+usd(med[r.Kind]))
	}
	top := outl[0]
	f := &SignalFilter{Key: FacetKind, Value: top.Kind, Label: "тип «" + label(meter.ByKind, top.Kind) + "»"}
	if top.MRIID > 0 {
		f = &SignalFilter{Key: FacetMR, Value: mrKey(top.ProjectPath, top.MRIID), Label: "MR " + mrRef(top.ProjectPath, top.MRIID)}
	}
	return &Signal{ID: "outlier", Level: "warn",
		Title:  fmt.Sprintf("%s дороже медианы своего типа в %d+ раза", runsWord(len(outl)), OutlierFactor),
		Detail: strings.Join(parts, "; ") + fmt.Sprintf(". Медиана — по успешным запускам типа, если их не меньше %d; запуски дешевле %s не считаются.", OutlierMinSamples, usd(OutlierMinUSD)),
		Runs:   len(outl), Cost: cost, CostNote: "в сумме", Filter: f}
}

func coldSignal(cur []meter.RunRecord) *Signal {
	var n int
	var cost float64
	byProject := map[string]float64{}
	for _, r := range cur {
		in := r.Usage.Input + r.Usage.CacheRead + r.Usage.CacheWrite
		if in < ColdMinInput || cacheShare(r.Usage) >= ColdMaxShare {
			continue
		}
		n++
		cost += r.CostUSD
		byProject[r.ProjectPath] += r.CostUSD
	}
	if n == 0 {
		return nil
	}
	s := &Signal{ID: "cold", Level: "info",
		Title: fmt.Sprintf("Холодный кэш: %s почти без чтения из кэша", runsWord(n)),
		Detail: fmt.Sprintf("Из кэша промптов прочитано меньше %d%% входа (при входе от %d тыс. токенов). Обычно это первый запуск в репозитории или истёкший кэш; если часто — системный промпт меняется от запуска к запуску.",
			int(ColdMaxShare*100), ColdMinInput/1000),
		Runs: n, Cost: cost, CostNote: "стоили"}
	if p := topKey(byProject); p != "" {
		s.Filter = &SignalFilter{Key: FacetProject, Value: p, Label: "репозиторий " + repoName(p)}
	}
	return s
}

// topKey is the key with the largest value (ties: smallest key), "" keys
// skipped.
func topKey(m map[string]float64) string {
	best, bv := "", -1.0
	for k, v := range m {
		if k == "" {
			continue
		}
		if v > bv || (v == bv && k < best) {
			best, bv = k, v
		}
	}
	return best
}

func failedSignal(cur []meter.RunRecord, knownModel func(string) bool) *Signal {
	classes := map[string]int{}
	byKind := map[string]float64{}
	var bad []string
	seen := map[string]bool{}
	var n int
	var cost float64
	for _, r := range cur {
		if r.OK {
			continue
		}
		n++
		cost += r.CostUSD
		c := r.ErrorClass
		if c == "" {
			c = "failed"
		}
		classes[c]++
		byKind[r.Kind]++
		if r.Model != "" && knownModel != nil && !knownModel(r.Model) && !seen[r.Model] {
			seen[r.Model] = true
			bad = append(bad, r.Model)
		}
	}
	if n == 0 {
		return nil
	}
	keys := make([]string, 0, len(classes))
	for k := range classes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s ×%d", errorClassLabel(k), classes[k]))
	}
	detail := strings.Join(parts, ", ") + "."
	s := &Signal{ID: "failed", Level: "bad", Title: runsWord(n) + " с ошибкой", Runs: n, Cost: cost, CostNote: "потрачено впустую"}
	if len(bad) > 0 {
		sort.Strings(bad)
		detail += " Модель «" + strings.Join(bad, "», «") + "» — не модель Claude (в имени нет opus, sonnet или haiku): проверьте модели в конфиге."
		s.Filter = &SignalFilter{Key: FacetModel, Value: bad[0], Label: "модель " + bad[0]}
	} else if k := topKey(byKind); k != "" {
		s.Filter = &SignalFilter{Key: FacetKind, Value: k, Label: "тип «" + label(meter.ByKind, k) + "»"}
	}
	s.Detail = detail
	return s
}

var errorClassLabels = map[string]string{
	"rate_limited": "лимит запросов", "timeout": "таймаут", "canceled": "отменён", "max_turns": "лимит ходов", "failed": "ошибка",
}

func errorClassLabel(c string) string {
	if l, ok := errorClassLabels[c]; ok {
		return l
	}
	return c
}

func noUserSignal(cur []meter.RunRecord) *Signal {
	var n int
	var cost, all float64
	for _, r := range cur {
		all += r.CostUSD
		if r.Username == "" {
			n++
			cost += r.CostUSD
		}
	}
	if n == 0 || n == len(cur) {
		// everything unattributed (or a "-" user filter): nothing to point at
		return nil
	}
	note := "0% стоимости"
	if all > 0 {
		note = strconv.FormatFloat(cost/all*100, 'f', 1, 64) + "% стоимости"
		note = strings.Replace(strings.Replace(note, ".0%", "%", 1), ".", ",", 1)
	}
	return &Signal{ID: "nouser", Level: "info", Title: "Без пользователя: " + runsWord(n),
		Detail: "Карта системы, обучение по закрытиям, разбор CI main-ветки и другие фоновые задачи ни на кого не записываются — их видно только здесь.",
		Runs:   n, Cost: cost, CostNote: note,
		Filter: &SignalFilter{Key: FacetUser, Value: UnattributedUser, Label: "без пользователя"}}
}

// ---------------------------------------------------------------------------
// budget
// ---------------------------------------------------------------------------

// Budget is the monthly budget block (usage.budget_usd > 0). It is global:
// every user and repository, whatever the page's filters.
type Budget struct {
	BudgetUSD  float64   `json:"budget_usd"`
	MonthStart time.Time `json:"month_start"`
	MonthEnd   time.Time `json:"month_end"`
	SpentUSD   float64   `json:"spent_usd"`
	// RatePerDayUSD is the average daily spend of the last 7 days.
	RatePerDayUSD float64 `json:"rate_per_day_usd"`
	ForecastUSD   float64 `json:"forecast_usd"`
	SpentShare    float64 `json:"spent_share"`
	ForecastShare float64 `json:"forecast_share"`
	DaysLeft      float64 `json:"days_left"`
	// HistoryDays is how many days of runs are stored (< 7: the rate
	// counts days without data as zero, so the forecast is rough).
	HistoryDays float64 `json:"history_days"`
	Rough       bool    `json:"rough"`
}

// monthBounds is the calendar month of now in loc.
func monthBounds(now time.Time, loc *time.Location) (start, end time.Time) {
	n := now.In(loc)
	start = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 1, 0)
}

// budgetOf builds the block from the month-to-date and last-7-days spend.
func budgetOf(limit, spent, week float64, now time.Time, loc *time.Location, first time.Time) *Budget {
	start, end := monthBounds(now, loc)
	b := &Budget{BudgetUSD: limit, MonthStart: start, MonthEnd: end, SpentUSD: spent, RatePerDayUSD: week / 7,
		DaysLeft: end.Sub(now).Hours() / 24}
	b.ForecastUSD = spent + b.RatePerDayUSD*b.DaysLeft
	b.SpentShare, b.ForecastShare = spent/limit, b.ForecastUSD/limit
	if !first.IsZero() {
		b.HistoryDays = now.Sub(first).Hours() / 24
	}
	b.Rough = b.HistoryDays < 7
	return b
}
