package homeweb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag/diagindex"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

type fakeStore struct {
	runs     meter.RunStats
	hours    []meter.RunStats
	byProj   []meter.RunGroup
	probs    []problems.Record
	rules    []learn.Rule
	mem      []memory.Section
	stats    map[string]learn.OutcomeStats
	summary  diagindex.Summary
	diagRows []diagindex.Row
	jobs     []queue.Stored
	pending  []queue.Pending

	since, hourSince, hourUntil, outSince time.Time
	diagFilters                           []diagindex.Filter

	failRuns, failHourly, failKnow, failProb, failDiag, failQueue bool
}

var errBoom = errors.New("boom")

func (f *fakeStore) RunTotals(_ context.Context, fl meter.RunFilter) (meter.RunStats, error) {
	f.since = fl.Since
	if f.failRuns {
		return meter.RunStats{}, errBoom
	}
	return f.runs, nil
}

func (f *fakeStore) RunsHourly(_ context.Context, since, until time.Time) ([]meter.RunStats, error) {
	f.hourSince, f.hourUntil = since, until
	if f.failHourly {
		return nil, errBoom
	}
	return f.hours, nil
}

func (f *fakeStore) RunsBy(_ context.Context, _ meter.RunFilter, dim meter.Dimension) ([]meter.RunGroup, error) {
	if f.failRuns || dim != meter.ByProject {
		return nil, errBoom
	}
	return f.byProj, nil
}

func (f *fakeStore) ListProblems(_ context.Context, fl problems.Filter) ([]problems.Record, error) {
	if f.failProb || fl.Status != problems.StatusNew {
		return nil, errBoom
	}
	return f.probs, nil
}

func (f *fakeStore) ListAllLearnings(context.Context, int) ([]learn.Rule, error) {
	if f.failKnow {
		return nil, errBoom
	}
	return f.rules, nil
}

func (f *fakeStore) ListAllMemory(context.Context) ([]memory.Section, error) { return f.mem, nil }

func (f *fakeStore) OutcomeStatsAll(_ context.Context, since time.Time) (map[string]learn.OutcomeStats, error) {
	f.outSince = since
	return f.stats, nil
}

func (f *fakeStore) DiagRunSummary(context.Context, time.Time) (diagindex.Summary, error) {
	if f.failDiag {
		return diagindex.Summary{}, errBoom
	}
	return f.summary, nil
}

func (f *fakeStore) ListDiagRuns(_ context.Context, fl diagindex.Filter) ([]diagindex.Row, error) {
	f.diagFilters = append(f.diagFilters, fl)
	if f.failDiag {
		return nil, errBoom
	}
	var out []diagindex.Row
	for _, r := range f.diagRows {
		if r.Status == fl.Status && (fl.Limit == 0 || len(out) < fl.Limit) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) ListJobs(context.Context) ([]queue.Stored, error) {
	if f.failQueue {
		return nil, errBoom
	}
	return f.jobs, nil
}

func (f *fakeStore) ListPending(context.Context) ([]queue.Pending, error) { return f.pending, nil }

type fakeMap struct{ x *sysmap.Index }

func (m fakeMap) Index() *sysmap.Index { return m.x }

var now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func get(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func amp(s string) string { return strings.ReplaceAll(s, "&", "&amp;") }

func full(st *fakeStore) Options {
	return Options{Store: st, Usage: true, Diag: true, Problems: true, Location: time.UTC, Now: func() time.Time { return now },
		Map: fakeMap{&sysmap.Index{Repos: []sysmap.RepoState{{Path: "g/api"}, {Path: "g/web"}},
			Entries: []sysmap.IndexedEntry{{Status: sysmap.Unresolved}, {Status: sysmap.Unresolved}, {}}}}}
}

func hours24(fill func(i int) meter.RunStats) []meter.RunStats {
	out := make([]meter.RunStats, 24)
	for i := range out {
		out[i] = fill(i)
	}
	return out
}

// richStore has something in every source.
func richStore() *fakeStore {
	return &fakeStore{
		runs: meter.RunStats{Runs: 12, Failed: 2, CostUSD: 1.234, Usage: core.TokenUsage{Input: 250_000, CacheRead: 750_000}},
		hours: hours24(func(i int) meter.RunStats {
			if i == 23 {
				return meter.RunStats{Runs: 3, Failed: 1, CostUSD: 0.5}
			}
			return meter.RunStats{Runs: int64(i % 2)}
		}),
		byProj: []meter.RunGroup{{Key: "g/tokens", RunStats: meter.RunStats{CostUSD: 0.2, Usage: core.TokenUsage{Input: 9e6}}},
			{Key: "g/costly", RunStats: meter.RunStats{CostUSD: 0.9}}, {Key: "", RunStats: meter.RunStats{CostUSD: 5}}},
		probs: []problems.Record{
			{Kind: problems.LabelsFailed, ProjectPath: "g/web", Subject: "label x", LastSeen: now.Add(-time.Hour), Count: 1},
			{Kind: problems.RepoConfigInvalid, ProjectPath: "g/api", Subject: ".reviewbot.yaml", LastSeen: now.Add(-10 * time.Minute), Count: 3},
			{Kind: problems.DiffTruncated, ProjectPath: "g/api", Subject: "diff", LastSeen: now.Add(-2 * time.Hour), Count: 1},
		},
		rules: make([]learn.Rule, 5),
		mem:   []memory.Section{{ProjectID: 1}, {ProjectID: 1}, {ProjectID: 2}},
		stats: map[string]learn.OutcomeStats{"g/api": {Fixed: 8, Dismissed: 1}, "g/web": {Fixed: 1, Dismissed: 2, Open: 9}},
		summary: diagindex.Summary{Total: 9, Last: now.Add(-7 * time.Minute),
			ByStatus: map[string]int{"ok": 5, "failed": 1, "interrupted": 1, "postponed": 2}},
		diagRows: []diagindex.Row{
			{ID: "20261028-110000-aaaa", StartedAt: now.Add(-time.Hour), Kind: string(core.JobReview), ProjectPath: "g/api", MRIID: 7,
				MRTitle: "feat: login", Status: "failed", Error: "SECRET-ERROR-TEXT", Reason: "SECRET-REASON"},
			{ID: "20261028-100000-bbbb", StartedAt: now.Add(-2 * time.Hour), Kind: string(core.JobReview), ProjectPath: "g/web", MRIID: 3,
				Status: "interrupted", Reason: "процесс бота перезапущен"},
			{ID: "20261027-130000-cccc", StartedAt: now.Add(-23 * time.Hour), Kind: string(core.JobReview), ProjectPath: "g/api", MRIID: 9,
				MRTitle: "fix: limits", Status: "postponed", RetryAt: now.Add(time.Hour), Reason: "SECRET-POSTPONE-REASON"},
			{ID: "20261028-115500-dddd", StartedAt: now.Add(-5 * time.Minute), ProjectPath: "G/Web", MRIID: 4, Status: "running"},
		},
		jobs: []queue.Stored{{Job: core.Job{Kind: core.JobReview}}, {Job: core.Job{Kind: core.JobReview}, StartedAt: now}, {}},
		pending: []queue.Pending{{Job: core.Job{Kind: core.JobReview, ProjectPath: "g/web", MRIID: 4}},
			{Job: core.Job{Kind: core.JobReview, ProjectPath: "g/api", MRIID: 1}}},
	}
}

func TestPage(t *testing.T) {
	st := richStore()
	h := New(full(st))
	w := get(h, "GET", "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		// navigation with the problems counter (one breaks: red)
		`<a href="/" aria-current="page">Главная</a>`, `href="/usage"`, `href="/runs"`, `href="/knowledge"`, `href="/map"`,
		`Проблемы <span class="cnt bad" title="активных проблем">3</span>`,
		// status line: last task, tasks of the day, the queue (the pending review of g/web !4 is running)
		"Последняя задача <b>7 мин назад</b>", "<b>9 задач</b> за 24 ч", "<b>2 в очереди</b>", ", выполняется <b>2</b>", `class="pulse live"`,
		// headline numbers
		"Запуски Claude за 24 ч", `<div class="val">12</div>`, `href="` + amp(failedHref) + `" class="bad">с ошибкой: 2</a>`,
		`<div class="val">$1.23</div>`, "кэш — 75% токенов · 1 млн токенов",
		`<div class="val">3</div>`, `href="` + breaksHref + `" class="bad">1 ломает ревью или main</a>`, " · 2 другие",
		`<div class="val">5</div>`, "точность замечаний 75% за 30 дн. · память: 2 проекта",
		// sparklines: one hour group per hour, the failed hour marked, hover labels
		`<svg class="spark"`, `viewBox="0 0 24 30.00"`, `<path class="fl"`, `data-label="11:00–сейчас — 3 запуска, с ошибкой 1"`,
		`<title>11:00–сейчас — $0.50</title>`, `data-label="12:00–13:00 — 0 запусков"`,
		// bars
		`<rect class="b-breaks" x="0" y="0" width="1" height="1"><title>ломают ревью или main: 1</title></rect>`,
		`<rect class="b-fixed" x="0" y="0" width="9" height="1">`, `<rect class="b-dismissed" x="9" y="0" width="3" height="1">`,
		// «Требует внимания»: problems first (by impact), failed and postponed tasks
		`id="t-problems" aria-controls="tp-problems" checked>`, `<label for="t-failed">Упавшие задачи <span class="n">2</span></label>`,
		`<label for="t-postponed">Отложенные за сутки <span class="n">2</span></label>`,
		`href="/problems?kind=repo_config_invalid&amp;project=g%2Fapi"><span class="sev error" title="ломает ревью или main"></span><span class="t">Конфиг .reviewbot.yaml невалиден`,
		"10 мин назад · 3 раза", `<span class="p">g/api</span> — .reviewbot.yaml`,
		`<a href="/problems">Все активные (3) →</a>`, `<a href="/problems?impact=breaks">Только ломающие (1) →</a>`,
		`href="/runs/20261028-110000-aaaa"`, "ревью · g/api !7", "11:00 · ошибка", "feat: login", "10:00 · прервана",
		`href="/runs/20261027-130000-cccc"`, "вчера, 13:00 · отложена", "fix: limits · повтор в 13:00", "ещё 1 не показана",
		`<a href="` + amp(failedHref) + `">Открыть в Диагностике →</a>`, `<a href="` + amp(postponedHref) + `">`,
		// cards
		`href="/usage?period=24h"`, "$1.23 за 24 ч", "больше всего g/costly — $0.90",
		"9 задач за 24 ч", `<span class="bad">2 сбоя</span>`,
		"3 активные", `<span class="bad">1 ломает ревью</span>`,
		"5 правил", "точность 75%", "2 репозитория", "2 неразрешённых вызова",
		`src="` + webassets.HomePath + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Tasks: never their error or reason text.
	for _, bad := range []string{"SECRET", "процесс бота перезапущен", "?theme", "?open", "?demo", "location.hash", "hashchange"} {
		if strings.Contains(body, bad) {
			t.Errorf("page shows %q", bad)
		}
	}
	// Problems by impact: the breaking one first.
	if i, j := strings.Index(body, "repo_config_invalid"), strings.Index(body, "diff_truncated"); i < 0 || j < 0 || i > j {
		t.Errorf("problem order %d %d", i, j)
	}
	if v := webui.InlineViolations(body); len(v) != 0 {
		t.Errorf("inline style/handler (blocked by the CSP): %q", v)
	}
	if want := now.Add(-24 * time.Hour); !st.since.Equal(want) || !st.hourSince.Equal(want) || !st.hourUntil.Equal(now) {
		t.Errorf("windows %v %v %v", st.since, st.hourSince, st.hourUntil)
	}
	if want := now.AddDate(0, 0, -30); !st.outSince.Equal(want) {
		t.Errorf("accuracy window %v", st.outSince)
	}
	for _, f := range st.diagFilters {
		if f.Status == "" || (f.Status != "running" && (f.Limit != maxRows || !f.Since.Equal(now.Add(-24*time.Hour)))) {
			t.Errorf("diag filter %+v", f)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'nonce-", "style-src 'nonce-", "form-action 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("csp %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe") || strings.Contains(csp, "'self'; style") || strings.Contains(csp, "connect-src") {
		t.Errorf("csp %q", csp)
	}
	if w := get(h, "HEAD", "/", nil); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD %d %d", w.Code, w.Body.Len())
	}
	if w := get(h, "POST", "/", nil); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST %d", w.Code)
	}
	if w := get(h, "GET", "/nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown path %d", w.Code)
	}
}

// Nothing happened: every block says so calmly.
func TestEmptyStates(t *testing.T) {
	st := &fakeStore{hours: hours24(func(int) meter.RunStats { return meter.RunStats{} }), summary: diagindex.Summary{ByStatus: map[string]int{}}}
	o := full(st)
	o.Map = fakeMap{&sysmap.Index{}}
	body := get(New(o), "GET", "/", nil).Body.String()
	for _, want := range []string{
		"Задач ещё не было", "<b>0 задач</b> за 24 ч", "очередь <b>пуста</b>", `class="pulse"`,
		`<div class="val">0</div>`, "без ошибок", "токенов не было", "всё проверено", "решений по замечаниям за 30 дн. нет · память: 0 проектов",
		"Активных проблем нет — всё, что бот проверяет, проверено.", "За сутки ни одна задача не упала.", "За сутки ни одна задача не откладывалась.",
		`<span class="n">0</span>`, "без сбоев", "0 активных", "0 правил", "0 репозиториев", "все вызовы разрешены",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, bad := range []string{`class="cnt`, `<div class="panel-f">`, `<svg class="bar2"`, "больше всего", `class="muted">точность`, `<span class="bad">`} {
		if strings.Contains(body, bad) {
			t.Errorf("empty page shows %q", bad)
		}
	}
	if v := webui.InlineViolations(body); len(v) != 0 {
		t.Errorf("inline: %q", v)
	}
	// Problems without a breaking one; a map not built yet shows no figure.
	st.probs = []problems.Record{{Kind: problems.LabelsFailed, LastSeen: now}}
	o.Map = fakeMap{}
	body = get(New(o), "GET", "/", nil).Body.String()
	for _, want := range []string{"ревью и main не ломают", `Проблемы <span class="cnt" title`, `href="/problems?kind=labels_failed"`, `class="sev info"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "все вызовы разрешены") || strings.Contains(body, "Только ломающие") {
		t.Error("figures of an unbuilt map or of no breaking problem")
	}
}

func TestSwitchedOff(t *testing.T) {
	st := richStore()
	o := full(st)
	o.Usage, o.Diag, o.Problems, o.Map = false, false, false, nil
	b := get(New(o), "GET", "/", nil).Body.String()
	for _, bad := range []string{`href="/usage`, `href="/runs`, `href="/problems`, `href="/map"`, "Запуски Claude", "Активные проблемы",
		`id="attn-h"`, "Последняя задача", `<svg class="spark"`, "Карта системы"} {
		if strings.Contains(b, bad) {
			t.Errorf("switched-off page shown: %q", bad)
		}
	}
	for _, want := range []string{`href="/knowledge"`, "Выучено правил", `class="hkpis n1"`, "<b>3 в очереди</b>"} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Without the diagnostics the failed-runs figure is not a link and the
	// pending review of a running task cannot be told apart: it waits.
	o.Usage = true
	b = get(New(o), "GET", "/", nil).Body.String()
	if !strings.Contains(b, `<span class="bad">с ошибкой: 2</span>`) || !strings.Contains(b, "<b>3 в очереди</b>") || strings.Contains(b, amp(failedHref)) {
		t.Error("usage without diagnostics")
	}
	// Problems only: their tab is the first and checked.
	o.Usage, o.Problems = false, true
	b = get(New(o), "GET", "/", nil).Body.String()
	if !strings.Contains(b, `id="t-problems" aria-controls="tp-problems" checked>`) || strings.Contains(b, `id="t-failed"`) || !strings.Contains(b, `class="hkpis n2"`) {
		t.Error("problems tab only")
	}
	// Diagnostics only: the failed tab is checked.
	o.Problems, o.Diag = false, true
	b = get(New(o), "GET", "/", nil).Body.String()
	if !strings.Contains(b, `id="t-failed" aria-controls="tp-failed" checked>`) || strings.Contains(b, `id="t-problems"`) {
		t.Error("diagnostics tabs only")
	}
}

func TestErrors(t *testing.T) {
	st := richStore()
	st.failRuns, st.failKnow, st.failProb, st.failDiag, st.failQueue = true, true, true, true, true
	b := get(New(full(st)), "GET", "/", nil).Body.String()
	if n := strings.Count(b, `<div class="val">—</div>`); n != 4 {
		t.Errorf("failures must show dashes: %d", n)
	}
	if n := strings.Count(b, "Не удалось прочитать — подробности в логах бота."); n != 3 {
		t.Errorf("tab errors: %d", n)
	}
	for _, bad := range []string{"активных", "5 правил", `<div class="val">$`, "задач за 24 ч", `class="cnt`, `id="status"`, "Все активные", `<svg class="spark"`} {
		if strings.Contains(b, bad) {
			t.Errorf("failed source shows %q", bad)
		}
	}
	if !strings.Contains(b, "2 репозитория") {
		t.Error("the map does not depend on the store")
	}
	// The hourly series alone failing hides the run figures too.
	st = richStore()
	st.failHourly = true
	if b := get(New(full(st)), "GET", "/", nil).Body.String(); strings.Count(b, `<div class="val">—</div>`) != 2 {
		t.Error("hourly failure")
	}
	// Many problems: the count says it is capped; long lists say how many are not shown.
	st = richStore()
	st.probs = make([]problems.Record, maxProblems)
	for i := range st.probs {
		st.probs[i] = problems.Record{Kind: problems.InvalidOutput, LastSeen: now}
	}
	b = get(New(full(st)), "GET", "/", nil).Body.String()
	for _, want := range []string{`<div class="val">1000&#43;</div>`, "ещё 995 не показаны", "1000 ломают ревью или main", "1000&#43; активных"} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestAuthAndRegister(t *testing.T) {
	o := full(&fakeStore{})
	o.Token = "page-token-123456"
	h := New(o)
	if w := get(h, "GET", "/", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	w := get(h, "GET", "/?token=page-token-123456", nil)
	if w.Code != http.StatusSeeOther || strings.Contains(w.Header().Get("Location"), "page-token") {
		t.Fatalf("login: %d %v", w.Code, w.Header())
	}
	if w := get(h, "GET", "/", map[string]string{"Authorization": "Bearer page-token-123456"}); w.Code != http.StatusOK {
		t.Errorf("bearer: %d", w.Code)
	}
	mux := http.NewServeMux()
	Register(mux, full(&fakeStore{}))
	mux.HandleFunc("/other", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	if w := get(mux, "GET", "/other", nil); w.Code != http.StatusTeapot {
		t.Errorf("a specific route must win: %d", w.Code)
	}
	if w := get(mux, "GET", "/", nil); w.Code != http.StatusOK {
		t.Errorf("home via mux: %d", w.Code)
	}
	// Defaults: no clock, logger or location given.
	if w := get(New(Options{Store: &fakeStore{}}), "GET", "/", nil); w.Code != http.StatusOK {
		t.Errorf("defaults: %d", w.Code)
	}
}

func TestFormat(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, "задач"}, {1, "задача"}, {2, "задачи"}, {4, "задачи"}, {5, "задач"}, {11, "задач"}, {12, "задач"}, {21, "задача"}, {22, "задачи"}, {111, "задач"}, {-1, "задача"}} {
		if got := plural(tc.n, "задача", "задачи", "задач"); got != tc.want {
			t.Errorf("plural(%d) = %q", tc.n, got)
		}
	}
	for _, tc := range []struct {
		n    int64
		want string
	}{{950, "950"}, {1499, "1 тыс."}, {12_400, "12 тыс."}, {1_000_000, "1 млн"}, {3_450_000, "3,5 млн"}, {12_600_000, "13 млн"}} {
		if got := short(tc.n); got != tc.want {
			t.Errorf("short(%d) = %q", tc.n, got)
		}
	}
	for _, tc := range []struct {
		v    float64
		want string
	}{{0, "$0.00"}, {0.001, "<$0.01"}, {1.236, "$1.24"}} {
		if got := usd(tc.v); got != tc.want {
			t.Errorf("usd(%v) = %q", tc.v, got)
		}
	}
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{{10 * time.Second, "только что"}, {5 * time.Minute, "5 мин назад"}, {3 * time.Hour, "3 ч назад"}, {72 * time.Hour, "3 дн. назад"}} {
		if got := ago(now.Add(-tc.d), now); got != tc.want {
			t.Errorf("ago(%v) = %q", tc.d, got)
		}
	}
	h := New(full(&fakeStore{})).(*handler)
	if got := h.clock(now.Add(-time.Hour), now); got != "11:00" {
		t.Errorf("clock today %q", got)
	}
	if got := h.clock(now.Add(-13*time.Hour), now); got != "вчера, 23:00" {
		t.Errorf("clock yesterday %q", got)
	}
	if got := h.clock(now.Add(12*time.Hour+10*time.Minute), now); got != "завтра, 00:10" {
		t.Errorf("clock tomorrow %q", got)
	}
}

func TestSparkAndBar(t *testing.T) {
	if buildSpark(nil, nil, nil) != nil || buildBar(part{n: 0}) != nil {
		t.Fatal("empty input must draw nothing")
	}
	s := buildSpark([]float64{0, 2, 1}, []bool{false, true}, []string{"a", "b"})
	if s.ViewBox != "0 0 3 30.00" || s.Line != "M0.50 28.00L1.50 4.00L2.50 16.00" || s.Marks != "M1.50 4.00h0" ||
		!strings.HasPrefix(s.Area, s.Line+"L2.50 30.00L0.50 30.00Z") || len(s.Hours) != 3 || s.Hours[1].Label != "b" || s.Hours[2].Label != "" ||
		s.Hours[2].X != "2" || s.Hours[2].Mid != "2.50" || s.Hours[1].Dot != "M1.50 4.00h0" {
		t.Errorf("spark %+v", s)
	}
	// All zero: a flat line at the bottom, no division by zero.
	if z := buildSpark([]float64{0, 0}, nil, nil); z.Line != "M0.50 28.00L1.50 28.00" || z.Marks != "" {
		t.Errorf("zero spark %+v", z)
	}
	b := buildBar(part{"a", "A", 2}, part{"b", "B", 0}, part{"c", "C", 3})
	if b.ViewBox != "0 0 5 1" || len(b.Parts) != 2 || b.Parts[1] != (barPart{Class: "c", X: "2", W: "3", Title: "C"}) {
		t.Errorf("bar %+v", b)
	}
}

func TestTopByCost(t *testing.T) {
	if topByCost(nil) != nil || topByCost([]meter.RunGroup{{Key: "a"}}) != nil {
		t.Error("nothing spent: no top")
	}
	gs := []meter.RunGroup{{Key: "tokens", RunStats: meter.RunStats{CostUSD: 1, Usage: core.TokenUsage{Input: 1e9}}},
		{Key: "", RunStats: meter.RunStats{CostUSD: 50}}, {Key: "cost", RunStats: meter.RunStats{CostUSD: 2}}, {Key: "tie", RunStats: meter.RunStats{CostUSD: 2}}}
	if top := topByCost(gs); top == nil || top.Key != "cost" {
		t.Errorf("top %+v", top)
	}
}

// home.js finds the figure's label by the classes the page renders.
func TestScriptMatchesMarkup(t *testing.T) {
	rec := httptest.NewRecorder()
	webassets.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, webassets.HomePath, nil))
	js := rec.Body.String()
	for _, want := range []string{`closest(".hkpi")`, `querySelector(".at")`, `closest(".spark .h")`, `"data-label"`, `"svg.spark"`} {
		if !strings.Contains(js, want) {
			t.Errorf("home.js lacks %s", want)
		}
	}
	body := get(New(full(richStore())), "GET", "/", nil).Body.String()
	for _, want := range []string{`<div class="hkpi" id="k-runs">`, `<span class="at" aria-hidden="true"></span>`, `<g class="h" data-label=`, `<svg class="spark"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}
