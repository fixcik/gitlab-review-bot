package usageweb

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *store.SQLite {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	for _, r := range []meter.ClaudeRun{
		{At: now.Add(-2 * time.Hour), Kind: "review", ProjectID: 1, ProjectPath: "group/api", MRIID: 7, MRTitle: "feat: cache <b>x</b>",
			Username: "alice", Usage: core.TokenUsage{Input: 100, Output: 50, CacheRead: 1000}, CostUSD: 1.25, OK: true},
		{At: now.Add(-3 * 24 * time.Hour), Kind: "chat", ProjectID: 2, ProjectPath: "group/web", MRIID: 3,
			Username: "bob", UserID: 11, Model: "sonnet", Usage: core.TokenUsage{Input: 10, Output: 5}, CostUSD: 0.1, OK: true},
		{At: now.Add(-time.Hour), Kind: "other", Usage: core.TokenUsage{Output: 1}, OK: true},
		{At: now.Add(-60 * 24 * time.Hour), Kind: "review", ProjectPath: "group/old", MRIID: 1, Username: "carol", Usage: core.TokenUsage{Input: 9}, OK: true},
	} {
		if err := s.RecordRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func newHandler(t *testing.T, o Options) http.Handler {
	t.Helper()
	if o.Store == nil {
		o.Store = newStore(t)
	}
	o.Now = func() time.Time { return now }
	o.Location = time.UTC
	o.GitLabURL = "https://gitlab.example.com/"
	return New(o)
}

func get(h http.Handler, target string, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDisabledIs404(t *testing.T) {
	h := newHandler(t, Options{Enabled: false, Token: "s3cret"})
	for _, p := range []string{PagePath, DataPath, PagePath + "?token=s3cret"} {
		if rec := get(h, p, map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
}

func TestOpenWithoutToken(t *testing.T) {
	h := newHandler(t, Options{Enabled: true})
	if rec := get(h, PagePath, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Расход токенов") {
		t.Fatalf("page: %d", rec.Code)
	}
	if rec := get(h, DataPath, nil); rec.Code != http.StatusOK {
		t.Fatalf("data: %d %s", rec.Code, rec.Body)
	}
}

func TestOpenAccessStartupWarning(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	Register(http.NewServeMux(), Options{Store: newStore(t), Enabled: true, Logger: log})
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "USAGE_TOKEN") {
		t.Fatalf("no warning: %s", buf.String())
	}
	buf.Reset()
	Register(http.NewServeMux(), Options{Store: newStore(t), Enabled: true, Token: "x", Logger: log})
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("unexpected warning with token: %s", buf.String())
	}
}

func TestTokenEnforced(t *testing.T) {
	h := newHandler(t, Options{Enabled: true, Token: "s3cret"})
	for _, tc := range []struct {
		name string
		path string
		hdr  map[string]string
	}{
		{"none", PagePath, nil},
		{"none data", DataPath, nil},
		{"wrong bearer", DataPath, map[string]string{"Authorization": "Bearer nope"}},
		{"basic", DataPath, map[string]string{"Authorization": "Basic czNjcmV0"}},
		{"wrong query", PagePath + "?token=nope", nil},
	} {
		if rec := get(h, tc.path, tc.hdr); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d", tc.name, rec.Code)
		}
	}
	if rec := get(h, PagePath, nil, &http.Cookie{Name: cookieName, Value: "00"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad cookie: %d", rec.Code)
	}
	if rec := get(h, DataPath, map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusOK {
		t.Fatalf("bearer: %d", rec.Code)
	}
}

func TestQueryTokenSetsCookieAndRedirects(t *testing.T) {
	h := newHandler(t, Options{Enabled: true, Token: "s3cret"})
	rec := get(h, PagePath+"?period=7d&token=s3cret", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "token") || !strings.HasPrefix(loc, PagePath) || !strings.Contains(loc, "period=7d") {
		t.Fatalf("location %q", loc)
	}
	cs := rec.Result().Cookies()
	if len(cs) != 1 || !cs[0].HttpOnly || cs[0].Name != cookieName || strings.Contains(cs[0].Value, "s3cret") {
		t.Fatalf("cookie: %+v", cs)
	}
	if rec := get(h, DataPath, nil, cs[0]); rec.Code != http.StatusOK {
		t.Fatalf("cookie auth: %d", rec.Code)
	}
}

func TestDataShape(t *testing.T) {
	h := newHandler(t, Options{Enabled: true})
	rec := get(h, DataPath+"?period=7d", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", rec.Code, rec.Header())
	}
	var d Data
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Totals.Runs != 3 || d.Totals.TotalTokens != 1150+15+1 || d.Totals.CostUSD != 1.35 || d.Filter.Period != "7d" {
		t.Fatalf("totals: %+v", d)
	}
	if len(d.ByUser) != 3 || d.ByUser[0].Key != "alice" || d.ByUser[0].Label != "@alice" || d.ByUser[2].Key != UnattributedUser || d.ByUser[2].Label != "—" {
		t.Fatalf("by_user: %+v", d.ByUser)
	}
	if len(d.ByProject) != 3 || len(d.ByKind) != 3 || len(d.ByModel) != 2 {
		t.Fatalf("groups: %+v %+v %+v", d.ByProject, d.ByKind, d.ByModel)
	}
	if len(d.ByDay) != 8 || d.ByDay[len(d.ByDay)-1].Key != "2026-10-24" || d.ByDay[len(d.ByDay)-1].Runs != 2 {
		t.Fatalf("by_day: %+v", d.ByDay)
	}
	if len(d.TopMRs) != 2 || d.TopMRs[0].URL != "https://gitlab.example.com/group/api/-/merge_requests/7" || d.TopMRs[0].Title != "feat: cache <b>x</b>" {
		t.Fatalf("top_mrs: %+v", d.TopMRs)
	}
	// Raw keys exist in the JSON.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, k := range []string{"since", "until", "totals", "by_user", "by_project", "by_kind", "by_model", "by_day", "top_mrs"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing %q", k)
		}
	}
	if !strings.Contains(string(raw["totals"]), `"cache_read_tokens"`) || !strings.Contains(string(raw["by_kind"]), `"cache_share"`) {
		t.Fatalf("totals keys: %s", raw["totals"])
	}
	// The prompt cache share per kind: cache_read / (input + cache_read + cache_write).
	for _, g := range d.ByKind {
		want := map[string]float64{"review": 1000.0 / 1100, "chat": 0, "other": 0}[g.Key]
		if g.CacheShare != want {
			t.Errorf("cache_share of %s = %v, want %v", g.Key, g.CacheShare, want)
		}
	}
}

func TestUserAndProjectFilter(t *testing.T) {
	h := newHandler(t, Options{Enabled: true})
	var d Data
	rec := get(h, DataPath+"?period=30d&user=bob", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Totals.Runs != 1 || len(d.ByUser) != 1 || d.ByUser[0].Key != "bob" || d.Filter.User == nil || *d.Filter.User != "bob" {
		t.Fatalf("user filter: %+v", d)
	}
	// A user filter splits the per-day series by repository.
	if len(d.ByDayProject) != 0 {
		t.Fatalf("bob has one repo, no split expected: %+v", d.ByDayProject)
	}
	var a Data
	_ = json.Unmarshal(get(h, DataPath+"?period=7d&user=alice", nil).Body.Bytes(), &a)
	if len(a.ByDayProject) != 0 {
		t.Fatalf("alice has one repo: %+v", a.ByDayProject)
	}
	rec = get(h, DataPath+"?user=-", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Totals.Runs != 1 || d.Totals.Output != 1 {
		t.Fatalf("unattributed filter: %+v", d.Totals)
	}
	rec = get(h, DataPath+"?project=group/web", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Totals.Runs != 1 || d.ByProject[0].Key != "group/web" {
		t.Fatalf("project filter: %+v", d)
	}
	rec = get(h, DataPath+"?since=2026-08-01&until=2026-08-31", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Totals.Runs != 1 || d.Filter.Period != "custom" || len(d.ByDay) != 31 {
		t.Fatalf("custom: %+v / %d days", d.Totals, len(d.ByDay))
	}
	for _, bad := range []string{"?period=1y", "?since=x", "?since=2026-10-10&until=2026-10-01", "?since=2020-01-01"} {
		if rec := get(h, DataPath+bad, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}
}

func TestPageIsSelfContainedAndHasNoData(t *testing.T) {
	h := newHandler(t, Options{Enabled: true})
	rec := get(h, PagePath, nil)
	body := rec.Body.String()
	// The page carries no run data or prompt text: everything comes from
	// data.json, which holds only metadata.
	for _, s := range []string{"feat: cache", "alice", "SECRET", "{{NONCE}}", "<link", "@import", "url(", `href="http`, `src="http`, "//cdn", "innerHTML"} {
		if strings.Contains(body, s) {
			t.Fatalf("page contains %q", s)
		}
	}
	// The external scripts are same-origin versioned assets only: the
	// embedded ECharts bundle, the filter bar and the page script.
	srcs := regexp.MustCompile(`<script\b[^>]*\bsrc="([^"]*)"`).FindAllStringSubmatch(body, -1)
	if len(srcs) != 3 || srcs[0][1] != webassets.EChartsPath || srcs[1][1] != webassets.FiltersPath || srcs[2][1] != webassets.UsagePath {
		t.Fatalf("script tags: %q", srcs)
	}
	if !strings.Contains(body, usageCSS) {
		t.Fatal("page does not inline usage.css")
	}
	// The look and the helpers are the shared ones (internal/webui), not a copy.
	css, js := webui.Assets()
	if !strings.Contains(body, css) || !strings.Contains(body, js) {
		t.Fatal("page does not inline webui.Assets")
	}
	for _, own := range []string{"--bg:", "THEME_KEY", "function h(tag", ".topbar {"} {
		if strings.Contains(pageHTML, own) {
			t.Errorf("page.html carries its own copy of shared code: %q", own)
		}
	}
	if v := webui.InlineViolations(body); len(v) != 0 {
		t.Errorf("inline style/handler (blocked by the CSP): %q", v)
	}
	// Every inline script and style carries this response's nonce.
	nonce := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(rec.Header().Get("Content-Security-Policy"))
	for _, tag := range regexp.MustCompile(`<(script|style)\b[^>]*>`).FindAllString(body, -1) {
		if nonce == nil || !strings.Contains(tag, `nonce="`+nonce[1]+`"`) {
			t.Errorf("tag without the nonce: %s", tag)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "script-src 'nonce-", "' 'self';", "base-uri 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("csp %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "http") {
		t.Fatalf("csp too loose: %q", csp)
	}
	for _, d := range strings.Split(csp, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "style-src") && strings.Contains(d, "'self'") {
			t.Fatalf("style-src widened: %q", d)
		}
	}
	if !strings.Contains(body, "по ценам API; при подписке это оценка, а не счёт") {
		t.Fatal("missing cost disclaimer")
	}
	if rec := get(h, "/usage/other", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("other path: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, DataPath, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", w.Code)
	}
}

func TestByDayProject(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, r := range []meter.ClaudeRun{
		{At: now.Add(-26 * time.Hour), Kind: "review", ProjectPath: "group/web", Username: "alice", Usage: core.TokenUsage{Input: 40}, CostUSD: 0.5, OK: true},
		{At: now.Add(-time.Hour), Kind: "memory", Username: "alice", Usage: core.TokenUsage{Input: 7}, OK: true},
	} {
		if err := s.RecordRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	h := newHandler(t, Options{Enabled: true, Store: s})
	var d Data
	if err := json.Unmarshal(get(h, DataPath+"?period=7d&user=alice", nil).Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	// Repositories in token order; the run without a repository is left to
	// the page's "other" remainder.
	if len(d.ByDayProject) != 2 || d.ByDayProject[0].Key != "group/api" || d.ByDayProject[1].Key != "group/web" {
		t.Fatalf("by_day_project: %+v", d.ByDayProject)
	}
	for _, sr := range d.ByDayProject {
		if len(sr.Days) != len(d.ByDay) {
			t.Fatalf("%s: %d days, by_day has %d", sr.Key, len(sr.Days), len(d.ByDay))
		}
	}
	last := len(d.ByDay) - 1
	if d.ByDayProject[0].Days[last].TotalTokens != 1150 || d.ByDayProject[1].Days[last-1].Input != 40 || d.ByDay[last].TotalTokens != 1157 {
		t.Fatalf("values: %+v / %+v", d.ByDayProject, d.ByDay[last])
	}
	var k bool
	for _, g := range d.ByKind {
		k = k || (g.Key == "memory" && g.Label == "Память проекта")
	}
	if !k {
		t.Fatalf("memory kind label: %+v", d.ByKind)
	}
	// No split without a user filter, or with a project filter.
	for _, q := range []string{"?period=7d", "?period=7d&user=alice&project=group/web"} {
		var x Data
		_ = json.Unmarshal(get(h, DataPath+q, nil).Body.Bytes(), &x)
		if len(x.ByDayProject) != 0 {
			t.Fatalf("%s: %+v", q, x.ByDayProject)
		}
	}
	if raw := get(h, DataPath+"?period=7d", nil).Body.String(); strings.Contains(raw, "by_day_project") {
		t.Fatal("by_day_project should be omitted")
	}
}

// Every page of the bot links to the other three.
func TestPageLinksToOtherPages(t *testing.T) {
	h := newHandler(t, Options{Enabled: true})
	body := get(h, PagePath, nil).Body.String()
	for _, l := range []string{"/runs", "/map", "/problems"} {
		if !strings.Contains(body, `href="`+l+`"`) {
			t.Errorf("the usage page does not link to %s", l)
		}
	}
}
