// Package usageweb serves the token consumption page: GET /usage (an HTML
// shell with the shared look of internal/webui; the page script, the
// filter bar and ECharts are same-origin files of internal/webassets, no
// external resources) and GET /usage/data.json (the aggregates, the
// previous period, the chart buckets, the signals and the budget, all
// computed here: the browser never gets the raw runs). Only run metadata is exposed: users, projects,
// MR titles, token counts and notional cost; never prompts, code or answers.
package usageweb

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// Paths served by the handler.
const (
	PagePath = "/usage"
	DataPath = "/usage/data.json"
)

// cookieName holds a hash of the access token after a ?token= login.
const cookieName = "reviewbot_usage"

// UnattributedUser is the "user" filter value for runs nobody is charged
// for (GitLab usernames cannot be "-").
const UnattributedUser = "-"

// DefaultModel is the "model" filter value for runs on the CLI's default
// model (no model requested, no per-model split).
const DefaultModel = "-"

// maxRecords bounds the per-run read behind the signals, the chart and the
// filter counts (current and previous period together).
const maxRecords = 200000

// maxRange bounds custom periods.
const maxRange = 400 * 24 * time.Hour

// Store is what the page reads (*store.SQLite).
type Store interface {
	RunTotals(ctx context.Context, f meter.RunFilter) (meter.RunStats, error)
	RunsBy(ctx context.Context, f meter.RunFilter, dim meter.Dimension) ([]meter.RunGroup, error)
	TopMRs(ctx context.Context, f meter.RunFilter, n int) ([]meter.MRStats, error)
	RunRecords(ctx context.Context, f meter.RunFilter, limit int) ([]meter.RunRecord, bool, error)
	FirstRunAt(ctx context.Context) (time.Time, error)
}

// Options configure the handler.
type Options struct {
	// Nav is the shared header (switched-off pages, problems badge).
	Nav   webui.Nav
	Store Store
	// Enabled=false makes both endpoints answer 404.
	Enabled bool
	// Token, when non-empty, is required (Bearer header, ?token= or the
	// cookie set after it). Empty = open access.
	Token string
	// GitLabURL is used for MR links, e.g. https://gitlab.example.com.
	GitLabURL string
	// Location splits days (default time.Local).
	Location *time.Location
	Now      func() time.Time
	Logger   *slog.Logger
	// TopN is how many MRs the "most expensive" table shows (default 20).
	TopN int
	// BudgetUSD > 0 adds the monthly budget block (usage.budget_usd).
	BudgetUSD float64
	// KnownModel reports whether a requested model name is a Claude model
	// (the "errors" signal names the others as a config mistake). nil:
	// every name is.
	KnownModel func(string) bool
}

type handler struct {
	o     Options
	guard *Guard
}

// New returns the handler for PagePath and DataPath (mount it on both).
func New(o Options) http.Handler {
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.TopN <= 0 {
		o.TopN = 20
	}
	o.GitLabURL = strings.TrimRight(o.GitLabURL, "/")
	return &handler{o: o, guard: NewGuard(o.Token, cookieName, PagePath, PagePath, "USAGE_TOKEN")}
}

// Register mounts the handler on mux and logs how the page is protected.
func Register(mux *http.ServeMux, o Options) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	h := New(o)
	mux.Handle(PagePath, h)
	mux.Handle(DataPath, h)
	switch {
	case !o.Enabled:
		o.Logger.Info("usage page disabled (usage.enabled: false)")
	case o.Token == "":
		o.Logger.Warn("usage page " + PagePath + " is open WITHOUT authentication: anyone who can reach the bot's port " +
			"sees users, repositories, MR titles and token consumption. To protect it set USAGE_TOKEN (or usage.token) " +
			"and open " + PagePath + "?token=<token> once; to turn it off set usage.enabled: false")
	default:
		o.Logger.Info("usage page enabled with token", "path", PagePath)
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.o.Enabled || (r.URL.Path != PagePath && r.URL.Path != DataPath) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	SecurityHeaders(w)
	if !h.guard.Authorize(w, r) {
		return
	}
	if r.URL.Path == DataPath {
		h.serveData(w, r)
		return
	}
	h.servePage(w, r)
}

//go:embed page.html
var pageHTML string

//go:embed usage.css
var usageCSS string

func (h *handler) servePage(w http.ResponseWriter, r *http.Request) {
	nonce := Nonce()
	SetChartPageCSP(w, nonce)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	css, js := webui.Assets()
	page := strings.NewReplacer("{{HEADER}}", h.o.Nav.Header(r.Context(), webui.SectionUsage), "{{COMMON_CSS}}", css, "{{USAGE_CSS}}", usageCSS, "{{COMMON_JS}}", js, "{{ECHARTS}}", webassets.EChartsPath,
		"{{FILTERS}}", webassets.FiltersPath, "{{USAGE_JS}}", webassets.UsagePath).Replace(pageHTML)
	_, _ = io.WriteString(w, strings.ReplaceAll(page, "{{NONCE}}", nonce))
}

// ---------------------------------------------------------------------------
// data.json
// ---------------------------------------------------------------------------

// Stats is one aggregate row in data.json.
type Stats struct {
	Runs       int64 `json:"runs"`
	Failed     int64 `json:"failed"`
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	// CacheShare is cache_read / (input + cache_read + cache_write): the
	// share of the input served from the prompt cache (0 without input).
	CacheShare  float64 `json:"cache_share"`
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
	DurationMS  int64   `json:"duration_ms"`
}

// Group is a row of a by_* table.
type Group struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	ID    int64  `json:"id,omitempty"`
	Stats
	// PrevCostUSD is the previous period's cost (by_user only).
	PrevCostUSD *float64 `json:"prev_cost_usd,omitempty"`
}

// MR is a row of top_mrs.
type MR struct {
	ProjectPath string `json:"project_path"`
	MRIID       int64  `json:"mr_iid"`
	Title       string `json:"title"`
	URL         string `json:"url,omitempty"`
	// Key is the "mr" filter value (path!iid).
	Key string `json:"key"`
	// Reviews is the number of review runs of the MR in the period.
	Reviews int64 `json:"reviews"`
	Stats
}

// Filter echoes the applied filter.
type Filter struct {
	Period  string  `json:"period"`
	Project string  `json:"project,omitempty"`
	User    *string `json:"user,omitempty"`
	Kind    string  `json:"kind,omitempty"`
	Model   *string `json:"model,omitempty"`
	MR      string  `json:"mr,omitempty"`
}

// Data is the data.json document.
type Data struct {
	Since     time.Time `json:"since"`
	Until     time.Time `json:"until"`
	Filter    Filter    `json:"filter"`
	GitLabURL string    `json:"gitlab_url,omitempty"`
	Totals    Stats     `json:"totals"`
	ByUser    []Group   `json:"by_user"`
	ByProject []Group   `json:"by_project"`
	ByKind    []Group   `json:"by_kind"`
	ByModel   []Group   `json:"by_model"`
	ByDay     []Group   `json:"by_day"`
	TopMRs    []MR      `json:"top_mrs"`
	// ByDayProject splits by_day by repository (the largest dayProjectsTop
	// ones). Only filled when a user filter is active and no project filter,
	// for the user's per-day chart.
	ByDayProject []DaySeries `json:"by_day_project,omitempty"`

	// Previous is the previous period of equal length (deltas, the dashed
	// series of the chart).
	Previous Previous `json:"previous"`
	// ReviewPerMR is the cost of reviewing one MR in the period.
	ReviewPerMR ReviewCost `json:"review_per_mr"`
	// AvgTurns is the average number of turns per run.
	AvgTurns float64 `json:"avg_turns"`
	// Series is the chart: 15 min, 30 min or day buckets.
	Series Series `json:"series"`
	// Signals: "На что обратить внимание", computed from the runs.
	Signals []Signal `json:"signals"`
	Rules   Rules    `json:"rules"`
	// Facets are the filter bar's options with counts.
	Facets map[string][]Facet `json:"facets"`
	// Partial: more than maxRecords runs; signals, the chart, the previous
	// period and the counts use the newest maxRecords only.
	Partial bool `json:"partial,omitempty"`
	// Budget is set only when usage.budget_usd > 0.
	Budget *Budget `json:"budget,omitempty"`
}

// DaySeries is one repository's per-day consumption, aligned with by_day.
type DaySeries struct {
	Key  string  `json:"key"`
	Days []Group `json:"days"`
}

// dayProjectsTop is how many repositories by_day_project splits out; the
// rest is "other" (by_day minus the series).
const dayProjectsTop = 5

func (h *handler) serveData(w http.ResponseWriter, r *http.Request) {
	f, flt, err := h.parseFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d, err := h.collect(r.Context(), f, flt)
	if err != nil {
		h.o.Logger.Error("usage: collect", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	_ = enc.Encode(d)
}

var periods = map[string]time.Duration{
	"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "90d": 90 * 24 * time.Hour,
}

// parseFilter reads period=24h|7d|30d|90d or since/until (RFC 3339 or
// YYYY-MM-DD; a date "until" is inclusive), project, user, kind, model
// ("-" = the default model) and mr (path!iid).
func (h *handler) parseFilter(q url.Values) (meter.RunFilter, Filter, error) {
	loc := h.o.Location
	now := h.o.Now().In(loc)
	f := meter.RunFilter{Location: loc, ProjectPath: strings.TrimSpace(q.Get("project")), Kind: strings.TrimSpace(q.Get("kind"))}
	flt := Filter{Project: f.ProjectPath, Kind: f.Kind}
	if q.Has("user") {
		u := strings.TrimPrefix(strings.TrimSpace(q.Get("user")), "@")
		flt.User = &u
		if u == UnattributedUser {
			u = ""
		}
		f.Username = &u
	}
	if m := strings.TrimSpace(q.Get("model")); m != "" {
		echo := m
		flt.Model = &echo
		if m == DefaultModel {
			m = ""
		}
		f.Model, f.HasModel = m, true
	}
	if mr := strings.TrimSpace(q.Get("mr")); mr != "" {
		p, n, ok := strings.Cut(mr, "!")
		iid, err := strconv.ParseInt(n, 10, 64)
		if !ok || p == "" || err != nil || iid <= 0 {
			return f, flt, fmt.Errorf("bad mr %q (want group/repo!iid)", mr)
		}
		f.MRProjectPath, f.MRIID, flt.MR = p, iid, mr
	}
	since, until := q.Get("since"), q.Get("until")
	if since == "" && until == "" {
		p := q.Get("period")
		if p == "" {
			p = "30d"
		}
		d, ok := periods[p]
		if !ok {
			return f, flt, fmt.Errorf("unknown period %q (24h, 7d, 30d, 90d)", p)
		}
		f.Since, f.Until, flt.Period = now.Add(-d), now, p
		return f, flt, nil
	}
	flt.Period = "custom"
	var err error
	if f.Until = now; until != "" {
		if f.Until, err = parseTime(until, loc, true); err != nil {
			return f, flt, fmt.Errorf("bad until: %w", err)
		}
	}
	if since != "" {
		if f.Since, err = parseTime(since, loc, false); err != nil {
			return f, flt, fmt.Errorf("bad since: %w", err)
		}
	} else {
		f.Since = f.Until.Add(-30 * 24 * time.Hour)
	}
	if !f.Since.Before(f.Until) {
		return f, flt, fmt.Errorf("since must be before until")
	}
	if f.Until.Sub(f.Since) > maxRange {
		return f, flt, fmt.Errorf("period longer than %d days", int(maxRange.Hours()/24))
	}
	return f, flt, nil
}

func parseTime(s string, loc *time.Location, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("want RFC 3339 or YYYY-MM-DD")
	}
	if endOfDay {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

func toStats(s meter.RunStats) Stats {
	return Stats{Runs: s.Runs, Failed: s.Failed, Input: s.Usage.Input, Output: s.Usage.Output,
		CacheRead: s.Usage.CacheRead, CacheWrite: s.Usage.CacheWrite, CacheShare: cacheShare(s.Usage), TotalTokens: s.Usage.Total(),
		CostUSD: s.CostUSD, DurationMS: s.DurationMS}
}

// cacheShare is the share of the input tokens read from the prompt cache.
func cacheShare(u core.TokenUsage) float64 {
	in := u.Input + u.CacheRead + u.CacheWrite
	if in <= 0 {
		return 0
	}
	return float64(u.CacheRead) / float64(in)
}

var kindLabels = map[string]string{
	"review": "Ревью", "chat": "Вопросы (чат)", "verification": "Перепроверка", "learning": "Обучение", "memory": "Память проекта", "map": "Карта системы", "ci": "Разбор CI", "other": "Прочее",
}

func label(dim meter.Dimension, key string) string {
	switch dim {
	case meter.ByUser:
		if key == "" {
			return "—"
		}
		return "@" + key
	case meter.ByKind:
		if l, ok := kindLabels[key]; ok {
			return l
		}
	case meter.ByModel:
		if key == "" {
			return "по умолчанию"
		}
	}
	if key == "" {
		return "—"
	}
	return key
}

func (h *handler) collect(ctx context.Context, f meter.RunFilter, flt Filter) (*Data, error) {
	d := &Data{Since: f.Since, Until: f.Until, Filter: flt, GitLabURL: h.o.GitLabURL}
	tot, err := h.o.Store.RunTotals(ctx, f)
	if err != nil {
		return nil, err
	}
	d.Totals = toStats(tot)
	for _, x := range []struct {
		dim meter.Dimension
		dst *[]Group
	}{{meter.ByUser, &d.ByUser}, {meter.ByProject, &d.ByProject}, {meter.ByKind, &d.ByKind}, {meter.ByModel, &d.ByModel}, {meter.ByDay, &d.ByDay}} {
		gs, err := h.o.Store.RunsBy(ctx, f, x.dim)
		if err != nil {
			return nil, err
		}
		out := make([]Group, 0, len(gs))
		for _, g := range gs {
			key := g.Key
			if x.dim == meter.ByUser && key == "" {
				key = UnattributedUser
			}
			out = append(out, Group{Key: key, Label: label(x.dim, g.Key), ID: g.ID, Stats: toStats(g.RunStats)})
		}
		*x.dst = out
	}
	d.ByDay = fillDays(d.ByDay, f.Since, f.Until, f.Location)
	if f.Username != nil && f.ProjectPath == "" && len(d.ByProject) > 1 {
		for _, p := range d.ByProject {
			if len(d.ByDayProject) == dayProjectsTop {
				break
			}
			if p.Key == "" { // runs without a repository cannot be filtered on
				continue
			}
			pf := f
			pf.ProjectPath = p.Key
			gs, err := h.o.Store.RunsBy(ctx, pf, meter.ByDay)
			if err != nil {
				return nil, err
			}
			days := make([]Group, 0, len(gs))
			for _, g := range gs {
				days = append(days, Group{Key: g.Key, Stats: toStats(g.RunStats)})
			}
			d.ByDayProject = append(d.ByDayProject, DaySeries{Key: p.Key, Days: fillDays(days, f.Since, f.Until, f.Location)})
		}
	}
	mrs, err := h.o.Store.TopMRs(ctx, f, h.o.TopN)
	if err != nil {
		return nil, err
	}
	if err := h.analyze(ctx, f, flt, d, mrs); err != nil {
		return nil, err
	}
	return d, nil
}

// analyze fills the per-run parts of d (analysis.go) from one bounded read
// of the runs of the current and the previous period.
func (h *handler) analyze(ctx context.Context, f meter.RunFilter, flt Filter, d *Data, mrs []meter.MRStats) error {
	w := newWindow(f.Since, f.Until)
	recs, partial, err := h.o.Store.RunRecords(ctx, meter.RunFilter{Since: w.prevSince, Until: w.until}, maxRecords)
	if err != nil {
		return err
	}
	first, err := h.o.Store.FirstRunAt(ctx)
	if err != nil {
		return err
	}
	cur := current(recs, f, w)
	d.Partial = partial
	d.Previous = previous(recs, f, w, first)
	d.ReviewPerMR = reviewCost(cur)
	d.AvgTurns = avgTurns(cur)
	var split []string
	for _, s := range d.ByDayProject {
		split = append(split, s.Key)
	}
	d.Series = buildSeries(recs, f, w, f.Location, split)
	d.Signals = signals(cur, h.o.KnownModel)
	if d.Signals == nil {
		d.Signals = []Signal{}
	}
	d.Rules = rules
	d.Facets = facets(recs, f, flt, w)
	prev := prevCostByUser(recs, f, w)
	for i := range d.ByUser {
		c := prev[d.ByUser[i].Key]
		d.ByUser[i].PrevCostUSD = &c
	}
	reviews := reviewsByMR(cur)
	d.TopMRs = make([]MR, 0, len(mrs))
	for _, m := range mrs {
		k := mrKey(m.ProjectPath, m.MRIID)
		d.TopMRs = append(d.TopMRs, MR{ProjectPath: m.ProjectPath, MRIID: m.MRIID, Title: m.Title, Key: k, Reviews: reviews[k],
			URL: h.mrURL(m.ProjectPath, m.MRIID), Stats: toStats(m.RunStats)})
	}
	if h.o.BudgetUSD > 0 {
		now := h.o.Now()
		start, _ := monthBounds(now, f.Location)
		spent, err := h.o.Store.RunTotals(ctx, meter.RunFilter{Since: start, Until: now})
		if err != nil {
			return err
		}
		week, err := h.o.Store.RunTotals(ctx, meter.RunFilter{Since: now.Add(-7 * 24 * time.Hour), Until: now})
		if err != nil {
			return err
		}
		d.Budget = budgetOf(h.o.BudgetUSD, spent.CostUSD, week.CostUSD, now, f.Location, first)
	}
	return nil
}

// fillDays returns one entry per calendar day of [since, until), zeros
// where nothing ran.
func fillDays(in []Group, since, until time.Time, loc *time.Location) []Group {
	have := make(map[string]Group, len(in))
	for _, g := range in {
		have[g.Key] = g
	}
	var out []Group
	s := since.In(loc)
	day := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, loc)
	for ; day.Before(until) && len(out) < 1000; day = day.AddDate(0, 0, 1) {
		k := day.Format("2006-01-02")
		g, ok := have[k]
		if !ok {
			g = Group{Key: k}
		}
		g.Label = day.Format("02.01")
		out = append(out, g)
	}
	return out
}

func (h *handler) mrURL(projectPath string, iid int64) string {
	if h.o.GitLabURL == "" || projectPath == "" || iid <= 0 {
		return ""
	}
	segs := strings.Split(strings.Trim(projectPath, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/%s/-/merge_requests/%d", h.o.GitLabURL, strings.Join(segs, "/"), iid)
}
