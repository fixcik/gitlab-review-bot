package webassets

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestServesECharts(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, EChartsPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("content type %q", ct)
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("versioned asset must be immutable: %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Body.Len() < 500_000 || !strings.Contains(rec.Body.String()[:400], "Apache Software Foundation") {
		t.Errorf("unexpected body (%d bytes)", rec.Body.Len())
	}
}

func TestLicenseAndUnknown(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"ECHARTS-LICENSE.txt", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Apache License") {
		t.Fatalf("license: %d", rec.Code)
	}
	for _, p := range []string{Prefix + "../webassets.go", Prefix + "static/echarts.min.js", Prefix + "nope.js"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EChartsPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

func TestServesFilters(t *testing.T) {
	if !regexp.MustCompile(`^/assets/filters-[0-9a-f]{12}\.js$`).MatchString(FiltersPath) {
		t.Fatalf("FiltersPath %q", FiltersPath)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, FiltersPath, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("filters: %d %v", rec.Code, rec.Header())
	}
	if !bytes.Equal(rec.Body.Bytes(), filtersJS) || !strings.Contains(rec.Body.String(), "ReviewFilters") {
		t.Errorf("unexpected body (%d bytes)", rec.Body.Len())
	}
	// HEAD: headers only.
	rec = httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodHead, FiltersPath, nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	// Only the current version is served; a changed file gets another URL.
	for _, p := range []string{Prefix + "filters.js", Prefix + "static/filters.js", Prefix + "filters-000000000000.js"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
	if contentVersion([]byte("a")) == contentVersion([]byte("b")) {
		t.Error("contentVersion must change with the content")
	}
}

// TestFiltersJSIsCSPSafe: the filter bar never builds markup from strings,
// never writes style attributes and never uses eval-like calls (the pages'
// CSP forbids inline styles and unsafe-eval).
func TestFiltersJSIsCSPSafe(t *testing.T) {
	src := string(filtersJS)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function",
		`setAttribute("style"`, "style.cssText", ".style.background", ".style.width", "setTimeout(\""} {
		if strings.Contains(src, bad) {
			t.Errorf("filters.js contains %q", bad)
		}
	}
}

func TestServesRunDetail(t *testing.T) {
	if !regexp.MustCompile(`^/assets/rundetail-[0-9a-f]{12}\.js$`).MatchString(RunDetailPath) {
		t.Fatalf("RunDetailPath %q", RunDetailPath)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RunDetailPath, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("rundetail: %d %v", rec.Code, rec.Header())
	}
	if !bytes.Equal(rec.Body.Bytes(), runDetailJS) || !strings.Contains(rec.Body.String(), "RunDetail") {
		t.Errorf("unexpected body (%d bytes)", rec.Body.Len())
	}
	for _, p := range []string{Prefix + "rundetail.js", Prefix + "static/rundetail.js", Prefix + "rundetail-000000000000.js"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
}

// The renderer inserts data only as text (no HTML sinks) and never writes
// a style attribute: the pages' CSP allows CSSOM, not style="".
func TestRunDetailHasNoHTMLSinks(t *testing.T) {
	src := string(runDetailJS)
	sinks := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*"(on|style)|style:\s*"`)
	if m := sinks.FindString(src); m != "" {
		t.Errorf("rundetail.js uses %s", m)
	}
	// Moved here from diagweb's TestNoHTMLInjectionSinks: the run page renders with this file.
	for _, want := range []string{"D.safeHref(j.mr_url)", "D.safeHref(d.trace_url)", `heavy: "тяжёлый"`} {
		if !strings.Contains(src, want) {
			t.Errorf("links from the data must go through D.safeHref: %s missing", want)
		}
	}
}

func TestServesMap(t *testing.T) {
	if !regexp.MustCompile(`^/assets/map-[0-9a-f]{12}\.js$`).MatchString(MapPath) {
		t.Fatalf("MapPath %q", MapPath)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MapPath, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("map: %d %v", rec.Code, rec.Header())
	}
	if !bytes.Equal(rec.Body.Bytes(), mapJS) || !strings.Contains(rec.Body.String(), "MapGraph") {
		t.Errorf("unexpected body (%d bytes)", rec.Body.Len())
	}
	for _, p := range []string{Prefix + "map.js", Prefix + "static/map.js", Prefix + "map-000000000000.js"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
}

// The graph inserts repository strings only as text and never writes a
// style attribute or an inline handler (the pages' CSP allows CSSOM and
// SVG presentation attributes, not style="" or on*=).
func TestMapHasNoHTMLSinks(t *testing.T) {
	src := string(mapJS)
	sinks := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*"(on|style)|style:\s*"|"on[a-z]+":|\bon[a-z]+:\s*function`)
	if m := sinks.FindString(src); m != "" {
		t.Errorf("map.js uses %s", m)
	}
	// The hint snippet quotes keys YAML would misread; the diagram is SVG
	// laid out by elkjs (layered, top to bottom, orthogonal, externals in
	// their own partition), reachable by keyboard (Tab, Enter, Esc).
	if strings.Contains(src, "echarts") {
		t.Error("map.js still uses ECharts")
	}
	for _, want := range []string{"JSON.stringify(v)", "RESERVED", "new global.ELK()", `"elk.direction": "DOWN"`, `"elk.edgeRouting": "ORTHOGONAL"`,
		`"elk.partitioning.partition": "1"`, `"tabindex", "0"`, `ev.key === "Escape"`, `ev.key === "Enter"`, "c4-rule", "по правилу"} {
		if !strings.Contains(src, want) {
			t.Errorf("map.js lacks %s", want)
		}
	}
}

func TestServesProblems(t *testing.T) {
	if !regexp.MustCompile(`^/assets/problems-[0-9a-f]{12}\.js$`).MatchString(ProblemsPath) {
		t.Fatalf("ProblemsPath %q", ProblemsPath)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ProblemsPath, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("problems: %d %v", rec.Code, rec.Header())
	}
	if !bytes.Equal(rec.Body.Bytes(), problemsJS) || !strings.Contains(rec.Body.String(), "ProblemsPage") {
		t.Errorf("unexpected body (%d bytes)", rec.Body.Len())
	}
	for _, p := range []string{Prefix + "problems.js", Prefix + "static/problems.js", Prefix + "problems-000000000000.js"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
}

// The /problems script inserts text only, writes no style attribute or
// inline handler and makes no requests (the page's CSP has no connect-src:
// hiding and returning are plain form POSTs).
func TestProblemsHasNoHTMLSinksOrNetwork(t *testing.T) {
	src := string(problemsJS)
	sinks := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*"(on|style)|style:\s*"|"on[a-z]+":|\bon[a-z]+:\s*function|\.style\b|\bfetch\(|XMLHttpRequest|sendBeacon|WebSocket|EventSource`)
	if m := sinks.FindString(src); m != "" {
		t.Errorf("problems.js uses %s", m)
	}
	for _, want := range []string{"textContent", "requestSubmit", "isSecureContext", `enhance(form, { submit: false })`} {
		if !strings.Contains(src, want) {
			t.Errorf("problems.js lacks %s", want)
		}
	}
}

func TestServesKnowledge(t *testing.T) {
	if !regexp.MustCompile(`^/assets/knowledge-[0-9a-f]{12}\.js$`).MatchString(KnowledgePath) {
		t.Fatalf("KnowledgePath %q", KnowledgePath)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, KnowledgePath, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("knowledge: %d %v", rec.Code, rec.Header())
	}
	if !bytes.Equal(rec.Body.Bytes(), knowledgeJS) || !strings.Contains(rec.Body.String(), "KnowledgePage") {
		t.Errorf("unexpected body (%d bytes)", rec.Body.Len())
	}
	for _, p := range []string{Prefix + "knowledge.js", Prefix + "static/knowledge.js", Prefix + "knowledge-000000000000.js"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
}

// The /knowledge script inserts text only, makes no requests and sets no
// style attribute: the only style access is CSSOM style.setProperty (the
// tooltip position), which the page's CSP allows.
func TestKnowledgeHasNoHTMLSinksOrNetwork(t *testing.T) {
	src := string(knowledgeJS)
	sinks := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*"(on|style)|"style"|style:\s*"|"on[a-z]+":|\bon[a-z]+:\s*function|\.style\s*=|\.style\.cssText|\.style\.[a-zA-Z]+\s*=|\bfetch\(|XMLHttpRequest|sendBeacon|WebSocket|EventSource|\.onclick`)
	if m := sinks.FindString(src); m != "" {
		t.Errorf("knowledge.js uses %s", m)
	}
	if n, m := strings.Count(src, ".style."), strings.Count(src, ".style.setProperty("); n != m || n == 0 {
		t.Errorf("style access other than setProperty: %d of %d", n-m, n)
	}
	for _, want := range []string{"textContent", "createElementNS", "JSON.parse", `form="kb-bulk"`, "data-max"} {
		if !strings.Contains(src, want) {
			t.Errorf("knowledge.js lacks %s", want)
		}
	}
}

func TestServesHome(t *testing.T) {
	if !regexp.MustCompile(`^/assets/home-[0-9a-f]{12}\.js$`).MatchString(HomePath) {
		t.Fatalf("HomePath %q", HomePath)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HomePath, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("home: %d %v", rec.Code, rec.Header())
	}
	if !bytes.Equal(rec.Body.Bytes(), homeJS) || !strings.Contains(rec.Body.String(), "HomePage") {
		t.Errorf("unexpected body (%d bytes)", rec.Body.Len())
	}
	rec = httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"home.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unversioned: %d", rec.Code)
	}
}

// The main page script writes text only: no HTML sinks, no requests, no
// style access at all.
func TestHomeHasNoHTMLSinksOrNetwork(t *testing.T) {
	src := string(homeJS)
	sinks := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(|\.style\b|\bfetch\(|XMLHttpRequest|sendBeacon|WebSocket|EventSource|\.on[a-z]+\s*=`)
	if m := sinks.FindString(src); m != "" {
		t.Errorf("home.js uses %s", m)
	}
	if !strings.Contains(src, "textContent") {
		t.Error("home.js lacks textContent")
	}
}

func TestServesUsage(t *testing.T) {
	if !regexp.MustCompile(`^/assets/usage-[0-9a-f]{12}\.js$`).MatchString(UsagePath) {
		t.Fatalf("UsagePath %q", UsagePath)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, UsagePath, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("usage: %d %v", rec.Code, rec.Header())
	}
	if !bytes.Equal(rec.Body.Bytes(), usageJS) || !strings.Contains(rec.Body.String(), "UsagePage") {
		t.Errorf("unexpected body (%d bytes)", rec.Body.Len())
	}
	for _, p := range []string{Prefix + "usage.js", Prefix + "static/usage.js", Prefix + "usage-000000000000.js"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
}

// The /usage script builds DOM with textContent only (ECharts tooltip
// markup escapes every data string), sets no style attribute or inline
// handler, styles data marks through CSSOM setProperty, and makes one kind
// of request: the same-origin /usage/data.json.
func TestUsageHasNoHTMLSinks(t *testing.T) {
	src := string(usageJS)
	sinks := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*"(on|style)|"style"|style:\s*"|"on[a-z]+":|\bon[a-z]+:\s*function|\.style\s*=|\.style\.cssText|\.style\.[a-zA-Z]+\s*=|XMLHttpRequest|sendBeacon|WebSocket|EventSource|\.onclick`)
	if m := sinks.FindString(src); m != "" {
		t.Errorf("usage.js uses %s", m)
	}
	if n, m := strings.Count(src, ".style."), strings.Count(src, ".style.setProperty("); n != m || n == 0 {
		t.Errorf("style access other than setProperty: %d of %d", n-m, n)
	}
	if n := strings.Count(src, "fetch("); n != 1 || !strings.Contains(src, `fetch("/usage/data.json?"`) {
		t.Errorf("requests: %d fetch calls", n)
	}
	for _, want := range []string{"textContent", "createElementNS", "esc(", "D.filters.mount", "history.pushState", "popstate"} {
		if !strings.Contains(src, want) {
			t.Errorf("usage.js lacks %s", want)
		}
	}
}

// The elkjs bundle of the /map diagram: versioned, immutable, with its
// licence; it runs in the page (no eval, no Function constructor).
func TestServesElk(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ElkPath, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") || !strings.Contains(rec.Body.String(), "ELK") {
		t.Fatalf("elk: %d %v", rec.Code, rec.Header())
	}
	if regexp.MustCompile(`\beval\(|new Function`).MatchString(rec.Body.String()) {
		t.Error("elk uses eval")
	}
	rec = httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"ELK-LICENSE.md", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Eclipse Public License") {
		t.Fatalf("elk license: %d", rec.Code)
	}
}
