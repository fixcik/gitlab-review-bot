package mapweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

type stubMap struct{ x *sysmap.Index }

func (s stubMap) Index() *sysmap.Index { return s.x }
func (s stubMap) RunStats(_ context.Context, since time.Time) (sysmap.RunStats, error) {
	if since.IsZero() {
		return sysmap.RunStats{Runs: 10, CostUSD: 2.5}, nil
	}
	return sysmap.RunStats{Runs: 3, Failed: 1, CostUSD: 0.3, Tokens: 1000}, nil
}
func (s stubMap) MaxRunsPerHour() int    { return 20 }
func (s stubMap) HintsList() [][2]string { return [][2]string{{"ORDERS_URL", "g/orders"}} }

const evil = `<img src=x onerror=alert(1)>`

func testIndex() *sysmap.Index {
	files := map[string][]sysmap.FileRecord{
		"g/orders": {{Path: "routes.py", Entries: []sysmap.Entry{{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "POST", Path: "/orders", File: "routes.py", Line: 3}}}},
		"g/web": {{Path: "src/api.ts", Entries: []sysmap.Entry{
			{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "POST", Path: "/orders", File: "src/api.ts", Line: 5, Raw: "`${API}/orders`"},
			{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: "/nowhere", File: "src/api.ts", Line: 9, Raw: evil, Base: evil},
		}}, {Path: "bad.ts", Error: "unusable"}},
	}
	repos := []sysmap.RepoState{
		{Path: "g/orders", SHA: "abc123", Branch: "main", ScannedAt: now, Status: sysmap.StatusOK},
		{Path: "g/web", SHA: "def456", Branch: "main", ScannedAt: now, Status: sysmap.StatusPartial, Pending: 2, Error: evil},
	}
	return sysmap.Build(repos, files, nil, now)
}

func newHandler(o Options) http.Handler {
	if o.Map == nil {
		o.Map = stubMap{x: testIndex()}
	}
	o.Now = func() time.Time { return now }
	o.GitLabURL = "https://gitlab.example.com/"
	return New(o)
}

func get(h http.Handler, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDisabledIs404(t *testing.T) {
	for _, h := range []http.Handler{newHandler(Options{Enabled: false}), New(Options{Enabled: true})} {
		for _, p := range []string{PagePath, DataPath} {
			if w := get(h, p, nil); w.Code != http.StatusNotFound {
				t.Errorf("%s: %d", p, w.Code)
			}
		}
	}
	if w := get(newHandler(Options{Enabled: true}), "/map/other", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown path: %d", w.Code)
	}
}

func TestAuth(t *testing.T) {
	h := newHandler(Options{Enabled: true, Token: "s3cret"})
	if w := get(h, DataPath, nil); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "MAP_TOKEN") {
		t.Fatalf("no token: %d %s", w.Code, w.Body.String())
	}
	if w := get(h, DataPath, map[string]string{"Authorization": "Bearer wrong"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", w.Code)
	}
	if w := get(h, DataPath, map[string]string{"Authorization": "Bearer s3cret"}); w.Code != http.StatusOK {
		t.Fatalf("bearer: %d", w.Code)
	}
	w := get(h, PagePath+"?token=s3cret", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != PagePath {
		t.Fatalf("login redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != cookieName || !c[0].HttpOnly || c[0].Path != PagePath {
		t.Fatalf("cookie: %+v", c)
	}
	if w := get(h, PagePath, map[string]string{"Cookie": c[0].Name + "=" + c[0].Value}); w.Code != http.StatusOK {
		t.Fatalf("cookie login: %d", w.Code)
	}
	if w := get(newHandler(Options{Enabled: true}), PagePath, nil); w.Code != http.StatusOK {
		t.Fatalf("open page: %d", w.Code)
	}
}

func TestDataShape(t *testing.T) {
	w := get(newHandler(Options{Enabled: true}), DataPath, nil)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if strings.Contains(w.Body.String(), "<img") {
		t.Error("JSON must escape HTML characters")
	}
	var d Data
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Repos) != 2 || d.Repos[0].Path != "g/orders" || d.Repos[0].URL != "https://gitlab.example.com/g/orders" || d.Repos[0].Provides != 1 {
		t.Errorf("repos = %+v", d.Repos)
	}
	web := d.Repos[1]
	if web.Status != sysmap.StatusPartial || web.Pending != 2 || web.Consumes != 2 || web.Unresolved != 1 || web.FileErrors != 1 || !web.InMap || web.Error != evil {
		t.Errorf("web = %+v", web)
	}
	if len(d.Edges) != 1 || d.Edges[0].From != "g/web" || d.Edges[0].To != "g/orders" || d.Edges[0].Counts[sysmap.HTTP] != 1 {
		t.Errorf("edges = %+v", d.Edges)
	}
	if d.ContractsTotal != 3 || len(d.Contracts) != 3 || d.UnresolvedTotal != 1 || len(d.Unresolved) != 1 {
		t.Fatalf("contracts %d/%d unresolved %d/%d", len(d.Contracts), d.ContractsTotal, len(d.Unresolved), d.UnresolvedTotal)
	}
	u := d.Unresolved[0]
	if u.Raw != evil || u.Base != evil || u.Reason == "" || u.URL != "https://gitlab.example.com/g/web/-/blob/def456/src/api.ts#L9" {
		t.Errorf("unresolved = %+v", u)
	}
	if d.Cost.MaxRunsPerHour != 20 || d.Cost.Runs30d != 3 || d.Cost.Failed30d != 1 || d.Cost.RunsAll != 10 || d.Cost.CostUSDAll != 2.5 || d.Cost.Tokens30d != 1000 {
		t.Errorf("cost = %+v", d.Cost)
	}
	if len(d.Hints) != 1 || d.Hints[0].Key != "ORDERS_URL" {
		t.Errorf("hints = %+v", d.Hints)
	}
}

func TestEmptyIndex(t *testing.T) {
	w := get(newHandler(Options{Enabled: true, Map: stubMap{}}), DataPath, nil)
	var d Data
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil || d.Repos == nil || d.Edges == nil || d.Unresolved == nil {
		t.Fatalf("empty data: %s %v", w.Body.String(), err)
	}
}

func TestPageIsSafe(t *testing.T) {
	w := get(newHandler(Options{Enabled: true}), PagePath, nil)
	body := w.Body.String()
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'nonce-") || strings.Contains(csp, "unsafe-inline") || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers: %v", w.Header())
	}
	for _, ph := range []string{"{{NONCE}}", "{{COMMON_CSS}}", "{{COMMON_JS}}", "{{ECHARTS}}", "{{FILTERS}}", "{{KIND_LABELS}}", "{{MAP_CSS}}", "{{MAPJS}}"} {
		if strings.Contains(body, ph) {
			t.Errorf("placeholder %s left", ph)
		}
	}
	if strings.Contains(body, "g/orders") || strings.Contains(body, "onerror") {
		t.Error("the page must carry no data (everything comes from data.json)")
	}
	sinks := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*"on`)
	if m := sinks.FindString(pageHTML); m != "" {
		t.Errorf("page uses %s", m)
	}
	if strings.Contains(pageHTML, ` style="`) {
		t.Error("inline style attribute (blocked by the CSP)")
	}
	if !strings.Contains(pageHTML, "D.safeHref(c.url)") || !strings.Contains(pageHTML, "D.safeHref(r.url)") {
		t.Error("links must go through safeHref")
	}
}

// Every page of the bot links to the other three.
func TestPageLinksToOtherPages(t *testing.T) {
	h := newHandler(Options{Enabled: true})
	body := get(h, PagePath, nil).Body.String()
	for _, l := range []string{"/usage", "/runs", "/problems"} {
		if !strings.Contains(body, `href="`+l+`"`) {
			t.Errorf("the map page does not link to %s", l)
		}
	}
}

// Every filter row is a filter bar over native controls: the repository
// picker (by default the repository with most contracts, «все» = every
// repository), the side (a choice, not clearable), the free-text filter of
// the action list (never collapses) and the protocol legend of the graph
// (a multi-select with the protocol colours as swatches).
func TestFilterBars(t *testing.T) {
	body := get(newHandler(Options{Enabled: true}), PagePath, nil).Body.String()
	for _, want := range []string{
		`src="` + webassets.FiltersPath + `"`, `id="contracts-bar"`, `data-clearable="false"`, `id="unres-bar"`, `data-collapse="false"`,
		`D.filters.enhance($("contracts-bar"))`, `contractsBar.refresh()`, `"data-short-label": sn(r.path)`,
		`localStorage.getItem(REPO_KEY)`, `var REPO_KEY = "reviewbot-map-repo"`,
		`id="proto-bar"`, `id="f-proto" multiple`, `data-swatch="s-in"`, `data-swatch="s-out"`, `data-swatch="s-cr"`, `data-swatch="s-cw"`,
		`D.filters.enhance($("proto-bar"))`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if v := webui.InlineViolations(body); len(v) != 0 {
		t.Errorf("inline style/handler (blocked by the CSP): %q", v)
	}
	if strings.Contains(body, "refreshSelect") || strings.Contains(body, "initCombos") {
		t.Error("leftovers of the old combobox")
	}
}

func decode(t *testing.T, h http.Handler) Data {
	t.Helper()
	w := get(h, DataPath, nil)
	var d Data
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// relationsIndex: g/orders provides POST /orders (code) and GET /orders/{id}
// twice (OpenAPI + code); g/web calls both, one more path through the
// ORDERS_URL hint that g/orders does not provide, and an unscanned
// repository through PAY_URL; g/admin calls GET /api/v1/orders/{id}
// (prefix match).
func relationsIndex() *sysmap.Index {
	files := map[string][]sysmap.FileRecord{
		"g/orders": {{Path: "app.py", Entries: []sysmap.Entry{
			{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "POST", Path: "/orders", File: "app.py", Line: 3, Source: sysmap.SourceCode},
			{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "GET", Path: "/orders/{id}", File: "openapi.yaml", Line: 9, Source: sysmap.SourceOpenAPI},
			{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "GET", Path: "/orders/{order_id}", File: "app.py", Line: 7, Source: sysmap.SourceCode},
			{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "GET", Path: "/health", File: "app.py", Line: 20, Source: sysmap.SourceCode},
		}}},
		"g/web": {{Path: "api.ts", Entries: []sysmap.Entry{
			{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "POST", Path: "/orders", File: "api.ts", Line: 1, Base: "ORDERS_URL"},
			{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: "/orders/{id}", File: "api.ts", Line: 2, Base: "ORDERS_URL"},
			{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: "/orders/export", File: "api.ts", Line: 3, Base: "ORDERS_URL"},
			{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: "/pay", File: "api.ts", Line: 4, Base: "PAY_URL"},
		}}},
		"g/admin": {{Path: "main.go", Entries: []sysmap.Entry{
			{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: "/api/v1/orders/{id}", File: "main.go", Line: 5},
		}}},
	}
	repos := []sysmap.RepoState{{Path: "g/orders", Status: sysmap.StatusOK}, {Path: "g/web", Status: sysmap.StatusOK}, {Path: "g/admin", Status: sysmap.StatusOK}}
	return sysmap.Build(repos, files, map[string]string{"ORDERS_URL": "g/orders", "PAY_URL": "g/pay"}, now)
}

func TestDataRelations(t *testing.T) {
	d := decode(t, newHandler(Options{Enabled: true, MaxHintKey: 200, Map: stubMap{x: relationsIndex()}}))
	if d.MaxHintKey != 200 {
		t.Errorf("max_hint_key = %d", d.MaxHintKey)
	}
	at := func(repo string, line int) Contract {
		t.Helper()
		for i, c := range d.Contracts {
			if c.ID != i {
				t.Fatalf("contract %d has id %d", i, c.ID)
			}
			if c.Repo == repo && c.Line == line {
				return c
			}
		}
		t.Fatalf("no contract %s:%d", repo, line)
		return Contract{}
	}
	ids := func(cs ...Contract) []int {
		out := []int{}
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	post, getAPI, getCode, health := at("g/orders", 3), at("g/orders", 9), at("g/orders", 7), at("g/orders", 20)
	for _, tc := range []struct {
		name       string
		c          Contract
		lands      []int
		noEndpoint bool
	}{
		{"hint, endpoint exists", at("g/web", 1), ids(post), false},
		{"hint, both descriptions of the endpoint", at("g/web", 2), ids(getAPI, getCode), false},
		{"hint, no such endpoint", at("g/web", 3), nil, true},
		{"hint to an unscanned repository", at("g/web", 4), nil, true},
		{"prefix match", at("g/admin", 5), ids(getAPI, getCode), false},
		{"a provider lands nowhere", health, nil, false},
	} {
		got := tc.c.Lands
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, tc.lands) || tc.c.NoEndpoint != tc.noEndpoint {
			t.Errorf("%s: lands %v no_endpoint %v, want %v %v", tc.name, tc.c.Lands, tc.c.NoEndpoint, tc.lands, tc.noEndpoint)
		}
	}
	if d.NoEndpointTotal != 2 || len(d.NoEndpoint) != 2 || d.NoEndpoint[0].Path != "/orders/export" || d.NoEndpoint[1].Target != "g/pay" {
		t.Errorf("no_endpoint = %d %+v", d.NoEndpointTotal, d.NoEndpoint)
	}
	if len(d.Endpoints) != 3 {
		t.Fatalf("endpoints = %+v", d.Endpoints)
	}
	byPath := map[string]Endpoint{}
	for _, e := range d.Endpoints {
		byPath[e.Method+" "+e.Path] = e
	}
	if e := byPath["GET /orders/{id}"]; !reflect.DeepEqual(e.Contracts, ids(getAPI, getCode)) || !reflect.DeepEqual(e.Callers, ids(at("g/admin", 5), at("g/web", 2))) ||
		e.CallersTotal != 2 || e.Repo != "g/orders" || e.Protocol != "http" {
		t.Errorf("GET /orders/{id} = %+v (OpenAPI + code merged, two callers)", e)
	}
	if e := byPath["GET /health"]; len(e.Callers) != 0 || e.CallersTotal != 0 || e.Callers == nil {
		t.Errorf("idle endpoint = %+v", e)
	}
	repo := map[string]Repo{}
	for _, r := range d.Repos {
		repo[r.Path] = r
	}
	if r := repo["g/orders"]; r.Endpoints != 3 || r.IdleEndpoints != 1 || r.Provides != 4 {
		t.Errorf("g/orders = %+v", r)
	}
	if r := repo["g/web"]; r.NoEndpoint != 2 || r.Endpoints != 0 {
		t.Errorf("g/web = %+v", r)
	}
	if r, ok := repo["g/pay"]; !ok || r.InMap {
		t.Errorf("the hint target outside the map is listed as an edge end: %+v", r)
	}
}

// Past the contracts cap, references only point at listed contracts and
// endpoints whose first contract is not listed are left out; the totals
// still count everything.
func TestDataRelationsCap(t *testing.T) {
	var prov []sysmap.Entry
	for i := 0; i < maxContracts; i++ {
		prov = append(prov, sysmap.Entry{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "GET", Path: fmt.Sprintf("/a%d", i), File: "a.go", Line: i + 1})
	}
	files := map[string][]sysmap.FileRecord{
		"g/a": {{Path: "a.go", Entries: prov}},
		// After the cap (repositories are indexed in name order).
		"g/b": {{Path: "b.go", Entries: []sysmap.Entry{
			{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "GET", Path: "/b", File: "b.go", Line: 1},
			{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: "/a0", File: "b.go", Line: 2},
		}}},
		"g/c": {{Path: "c.go", Entries: []sysmap.Entry{{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: "/b", File: "c.go", Line: 1}}}},
	}
	x := sysmap.Build([]sysmap.RepoState{{Path: "g/a"}, {Path: "g/b"}, {Path: "g/c"}}, files, nil, now)
	d := decode(t, newHandler(Options{Enabled: true, Map: stubMap{x: x}}))
	if len(d.Contracts) != maxContracts || d.ContractsTotal != maxContracts+3 {
		t.Fatalf("contracts %d/%d", len(d.Contracts), d.ContractsTotal)
	}
	if len(d.Endpoints) != maxContracts {
		t.Errorf("endpoints = %d, want only the listed ones", len(d.Endpoints))
	}
	a0 := d.Endpoints[0]
	if a0.Path != "/a0" || len(a0.Callers) != 0 || a0.CallersTotal != 1 {
		t.Errorf("/a0 = %+v: the caller is past the cap", a0)
	}
	var b Repo
	for _, r := range d.Repos {
		if r.Path == "g/b" {
			b = r
		}
	}
	if b.Endpoints != 1 || b.IdleEndpoints != 0 {
		t.Errorf("g/b = %+v: counts include contracts past the cap", b)
	}
}

// The diagram is SVG drawn by map.js (webassets.MapPath) with the layout
// of the embedded elkjs, which loads before it, under the nonce CSP of the
// chart pages; ECharts is not loaded on /map. No test hooks of the mockup
// (?theme, ?open, ?demo, hash state) are left.
func TestPageAssets(t *testing.T) {
	w := get(newHandler(Options{Enabled: true}), PagePath, nil)
	body := w.Body.String()
	ec, mp := strings.Index(body, `src="`+webassets.ElkPath+`"`), strings.Index(body, `src="`+webassets.MapPath+`"`)
	if ec < 0 || mp < 0 || ec > mp || strings.Contains(body, webassets.EChartsPath) {
		t.Error("the page loads the embedded elkjs and then map.js, not ECharts")
	}
	if strings.Contains(body, "cdn") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'nonce-") {
		t.Error("ECharts comes from the binary under the nonce CSP")
	}
	if strings.Index(body, "var D = (function") > mp {
		t.Error("map.js needs common.js loaded before it")
	}
	if !strings.Contains(body, ".gwrap") || !strings.Contains(body, ".act-list") || strings.Contains(body, ".g-edge") || strings.Contains(body, ".mtip") {
		t.Error("map.css is not inlined")
	}
	for _, hook := range []string{"location.hash", "location.search", "URLSearchParams", "?theme", "demo"} {
		if strings.Contains(pageHTML, hook) {
			t.Errorf("mock-only hook %q in the page", hook)
		}
	}
	// The diagram: a details panel for blocks and arrows (endpoints and the
	// calling files), Esc closes it, external systems as blocks, a note
	// when the layout library is missing.
	for _, want := range []string{"MapGraph.draw(", "MapGraph.snippet(data.hints", "data.no_endpoint", "data.endpoints",
		`id="gpanel"`, `id="graph-off"`, "onNode: openNode", "onEdge: openEdge", "onClose: closePanel", `ev.key === "Escape"`,
		`"ext:" + (g.host || g.service)`, "c.status === status", "callerChips(e)"} {
		if !strings.Contains(pageHTML, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	sinks := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|setAttribute\(\s*"(on|style)|\bon[a-z]+:\s*function|"on[a-z]+":`)
	if m := sinks.FindString(pageHTML); m != "" {
		t.Errorf("page uses %s", m)
	}
}

// externalIndex: g/web and g/app call Yandex OAuth (one marked by the
// model, one by a literal URL in an old record), g/web also calls S3 and
// one path nobody provides.
func externalIndex() *sysmap.Index {
	c := func(path, raw, host, service string, line int) sysmap.Entry {
		return sysmap.Entry{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "POST", Path: path, Raw: raw, File: "api.ts", Line: line,
			ExternalHost: host, ExternalService: service, Source: sysmap.SourceCode}
	}
	files := map[string][]sysmap.FileRecord{
		"g/web": {{Path: "api.ts", Entries: []sysmap.Entry{
			c("/token", "https://oauth.yandex.ru/token", "oauth.yandex.ru", "Yandex OAuth", 1),
			c("/info", evil, "oauth.yandex.ru", "", 2),
			c("/bucket", "https://s3.amazonaws.com/bucket", "", "", 3),
			c("/nowhere", "`${API}/nowhere`", "", "", 4),
		}}},
		"g/app": {{Path: "api.ts", Entries: []sysmap.Entry{c("/token", "'https://oauth.yandex.ru/token'", "", "", 7)}}},
	}
	repos := []sysmap.RepoState{{Path: "g/app", SHA: "a1", Status: sysmap.StatusOK}, {Path: "g/web", SHA: "w1", Status: sysmap.StatusOK}}
	return sysmap.Build(repos, files, nil, now)
}

func TestDataExternal(t *testing.T) {
	d := decode(t, newHandler(Options{Enabled: true, Map: stubMap{x: externalIndex()}}))
	if d.UnresolvedTotal != 1 || len(d.Unresolved) != 1 || d.Unresolved[0].Path != "/nowhere" {
		t.Errorf("unresolved = %d %+v", d.UnresolvedTotal, d.Unresolved)
	}
	if d.ExternalTotal != 4 || len(d.External) != 2 {
		t.Fatalf("external = %d %+v", d.ExternalTotal, d.External)
	}
	ya, s3 := d.External[0], d.External[1]
	if ya.Host != "oauth.yandex.ru" || ya.Service != "Yandex OAuth" || ya.Total != 3 || len(ya.Sites) != 3 ||
		!reflect.DeepEqual(ya.Repos, []ExternalRepo{{Repo: "g/app", Count: 1}, {Repo: "g/web", Count: 2}}) {
		t.Errorf("yandex = %+v", ya)
	}
	if s := ya.Sites[0]; s.Repo != "g/app" || s.Status != sysmap.External || s.ExternalHost != "oauth.yandex.ru" || s.URL != "https://gitlab.example.com/g/app/-/blob/a1/api.ts#L7" {
		t.Errorf("site = %+v", s)
	}
	if s3.Host != "s3.amazonaws.com" || s3.Service != "" || s3.Total != 1 || len(s3.Repos) != 1 {
		t.Errorf("s3 = %+v", s3)
	}
	for _, r := range d.Repos {
		if r.Path == "g/web" && (r.External != 3 || r.Unresolved != 1) || r.Path == "g/app" && (r.External != 1 || r.Unresolved != 0) {
			t.Errorf("repo = %+v", r)
		}
	}
	empty := decode(t, newHandler(Options{Enabled: true, Map: stubMap{}}))
	if empty.External == nil {
		t.Error("external must be a list")
	}
}

func TestDataExternalCap(t *testing.T) {
	var es []sysmap.Entry
	for i := 0; i < maxUnresolved+4; i++ { // a tie: the host decides the order
		host := "api.example.org"
		if i%2 == 1 {
			host = "cdn.example.net"
		}
		es = append(es, sysmap.Entry{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Path: fmt.Sprintf("/x/%d", i), File: "a.ts", Line: i + 1, ExternalHost: host})
	}
	x := sysmap.Build(nil, map[string][]sysmap.FileRecord{"g/web": {{Path: "a.ts", Entries: es}}}, nil, now)
	d := decode(t, newHandler(Options{Enabled: true, Map: stubMap{x: x}}))
	n := 0
	for _, g := range d.External {
		n += len(g.Sites)
	}
	if d.ExternalTotal != maxUnresolved+4 || n != maxUnresolved || d.External[0].Total != d.External[1].Total {
		t.Errorf("cap: total %d, listed %d, groups %+v", d.ExternalTotal, n, len(d.External))
	}
	if d.External[0].Host != "api.example.org" {
		t.Errorf("order: by host on a tie, got %s", d.External[0].Host)
	}
}

func TestPageExternalBlock(t *testing.T) {
	body := get(newHandler(Options{Enabled: true}), PagePath, nil).Body.String()
	for _, want := range []string{`id="h-ext"`, "Внешние зависимости", `id="ext"`, `renderExternal()`, `c.status === "external"`,
		`"` + sysmap.ViaEventSuffix + `": "по окончанию имени события"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// An endpoint is "spec only" when only an OpenAPI/Swagger file describes it
// and its repository serves no route in code (a frontend keeping the
// backend's swagger.json); a spec-only endpoint of a repository with code
// routes is not marked.
func TestDataSpecOnly(t *testing.T) {
	sp := func(path, file string) sysmap.Entry {
		return sysmap.Entry{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "GET", Path: path, File: file, Line: 1, Source: sysmap.SourceOpenAPI}
	}
	code := func(path string) sysmap.Entry {
		return sysmap.Entry{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "GET", Path: path, File: "app.php", Line: 2, Source: sysmap.SourceCode}
	}
	files := map[string][]sysmap.FileRecord{
		"g/backend":  {{Path: "app.php", Entries: []sysmap.Entry{code("/projects"), sp("/projects", "api.yaml"), sp("/legacy", "api.yaml")}}},
		"g/frontend": {{Path: "swagger.json", Entries: []sysmap.Entry{sp("/projects", "swagger.json"), sp("/users", "swagger.json")}}},
	}
	x := sysmap.Build([]sysmap.RepoState{{Path: "g/backend"}, {Path: "g/frontend"}}, files, nil, now)
	d := decode(t, newHandler(Options{Enabled: true, Map: stubMap{x: x}}))
	got := map[string]bool{}
	for _, e := range d.Endpoints {
		got[e.Repo+" "+e.Path] = e.SpecOnly
	}
	want := map[string]bool{"g/backend /projects": false, "g/backend /legacy": false, "g/frontend /projects": true, "g/frontend /users": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spec_only = %v", got)
	}
	for _, r := range d.Repos {
		if r.SpecOnly != (r.Path == "g/frontend") {
			t.Errorf("repo %s spec_only = %v", r.Path, r.SpecOnly)
		}
	}
	body := get(newHandler(Options{Enabled: true}), PagePath, nil).Body.String()
	for _, s := range []string{"только по спецификации", "e.spec_only", "r.spec_only"} {
		if !strings.Contains(body, s) {
			t.Errorf("page lacks %q", s)
		}
	}
}

// Calls linked by a topology rule: on the edge apart from the code
// matches, not in «Требуют действия», counted per rule (a rule that
// linked nothing has 0 hits).
func TestDataTopology(t *testing.T) {
	cons := func(path string, line int) sysmap.Entry {
		return sysmap.Entry{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: path, File: "api.ts", Line: line, Base: "backendHost"}
	}
	files := map[string][]sysmap.FileRecord{
		"g/back":  {{Path: "app.php", Entries: []sysmap.Entry{{Side: sysmap.Provides, Protocol: sysmap.HTTP, Method: "GET", Path: "/known", File: "app.php", Line: 1}}}},
		"g/front": {{Path: "api.ts", Entries: []sysmap.Entry{cons("/known", 1), cons("/unknown", 2), cons("/other", 3)}}},
	}
	rules := []sysmap.TopologyRule{{From: "g/front", To: "g/back", Via: []sysmap.Protocol{sysmap.HTTP}}, {From: "g/back", To: "g/front", Via: []sysmap.Protocol{sysmap.SSE}}}
	x := sysmap.BuildTopology([]sysmap.RepoState{{Path: "g/back"}, {Path: "g/front"}}, files, nil, rules, now)
	d := decode(t, newHandler(Options{Enabled: true, Map: stubMap{x: x}}))
	if d.UnresolvedTotal != 0 || len(d.Unresolved) != 0 || d.NoEndpointTotal != 0 {
		t.Errorf("rule calls must not need action: unresolved %d, no endpoint %d", d.UnresolvedTotal, d.NoEndpointTotal)
	}
	if len(d.Edges) != 1 || d.Edges[0].Total != 1 || d.Edges[0].RuleTotal != 2 || d.Edges[0].RuleCounts[sysmap.HTTP] != 2 {
		t.Errorf("edges = %+v", d.Edges)
	}
	want := []TopologyRule{{From: "g/front", To: "g/back", Via: []string{"http"}, Hits: 2}, {From: "g/back", To: "g/front", Via: []string{"sse"}, Hits: 0}}
	if !reflect.DeepEqual(d.Topology, want) {
		t.Errorf("topology = %+v", d.Topology)
	}
	for _, r := range d.Repos {
		if r.Path == "g/front" && (r.ByRule != 2 || r.Unresolved != 0) {
			t.Errorf("front = %+v", r)
		}
	}
	if empty := decode(t, newHandler(Options{Enabled: true, Map: stubMap{}})); empty.Topology == nil {
		t.Error("topology must be a list")
	}
	body := get(newHandler(Options{Enabled: true}), PagePath, nil).Body.String()
	for _, s := range []string{`id="h-topo"`, "Топология", `id="topo"`, "правило не сработало", `c.status === "topology"`, "rule_counts", "по правилу"} {
		if !strings.Contains(body, s) {
			t.Errorf("page lacks %q", s)
		}
	}
}

// Calls of a configured external system without a host group by its name.
func TestDataConfiguredExternal(t *testing.T) {
	c := func(path, file string, line int) sysmap.Entry {
		return sysmap.Entry{Side: sysmap.Consumes, Protocol: sysmap.HTTP, Method: "GET", Path: path, File: file, Line: line}
	}
	files := map[string][]sysmap.FileRecord{"g/back": {{Path: "x", Entries: []sysmap.Entry{
		c("/rest/a", "components/bitrix/A.php", 1), c("/rest/b", "components/bitrix/B.php", 2), c("/rest/api/2/search", "components/jira/J.php", 3),
		c("/c", "components/confluence/C.php", 4), c("/d", "components/confluence/D.php", 5)}}}}
	exts := []sysmap.ExternalRule{{Name: "Bitrix24", Repo: "g/back", Paths: []string{"components/bitrix/**"}}, {Name: "Jira", Repo: "g/back", Paths: []string{"components/jira/**"}, Host: "jira.example.com"},
		{Name: "Confluence", Repo: "g/back", Paths: []string{"components/confluence/**"}}}
	x := sysmap.BuildConfigured([]sysmap.RepoState{{Path: "g/back"}}, files, nil, nil, exts, now)
	d := decode(t, newHandler(Options{Enabled: true, Map: stubMap{x: x}}))
	if d.UnresolvedTotal != 0 || d.ExternalTotal != 5 || len(d.External) != 3 {
		t.Fatalf("unresolved %d external %d %+v", d.UnresolvedTotal, d.ExternalTotal, d.External)
	}
	// Equal totals without a host: by name.
	if g := d.External[0]; g.Host != "" || g.Service != "Bitrix24" || g.Total != 2 {
		t.Errorf("bitrix = %+v", g)
	}
	if g := d.External[1]; g.Host != "" || g.Service != "Confluence" || g.Total != 2 {
		t.Errorf("confluence = %+v", g)
	}
	if g := d.External[2]; g.Host != "jira.example.com" || g.Service != "Jira" || g.Total != 1 {
		t.Errorf("jira = %+v", g)
	}
	body := get(newHandler(Options{Enabled: true}), PagePath, nil).Body.String()
	if !strings.Contains(body, `"ext:" + (g.host || g.service)`) {
		t.Error("hostless external systems need their own diagram block")
	}
}
