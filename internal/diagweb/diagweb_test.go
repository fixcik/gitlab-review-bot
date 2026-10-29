package diagweb

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

type fixture struct {
	h      http.Handler
	reader *diag.Reader
	ids    map[string]string // label -> run id
}

type stubRunner struct{}

func (stubRunner) Exec(context.Context, core.ExecRequest) (*core.ExecResult, error) {
	return &core.ExecResult{}, nil
}
func (stubRunner) Run(context.Context, core.RunRequest) (*core.RunResult, error) {
	return &core.RunResult{Output: `{"reply":"<img src=x onerror=alert(1)>"}`, NumTurns: 1, Usage: core.TokenUsage{Input: 5, Output: 7}}, nil
}

func newFixture(t *testing.T, o Options) *fixture {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clock := now.Add(-3 * time.Hour)
	rec, err := diag.New(diag.Options{Root: filepath.Join(t.TempDir(), "diag"), Index: st, Now: func() time.Time { return clock },
		GitLabURL: "https://gitlab.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{reader: rec.Reader(), ids: map[string]string{}}
	runner := rec.WrapRunner(stubRunner{})
	add := func(label string, j core.Job, h core.Handler) {
		_ = rec.Handler(func(ctx context.Context, j core.Job) error {
			f.ids[label] = diag.FromContext(ctx).ID()
			return h(ctx, j)
		})(context.Background(), j)
		clock = clock.Add(time.Hour)
	}
	add("ok", core.Job{Kind: core.JobChat, ProjectPath: "group/api", MRIID: 7, Username: "alice"}, func(ctx context.Context, j core.Job) error {
		_, err := runner.Run(ctx, core.RunRequest{Dir: "/x", Prompt: "SECRET PROMPT <b>bold</b>"})
		return err
	})
	add("failed", core.Job{Kind: core.JobAutoReview, ProjectPath: "group/web", MRIID: 3}, func(ctx context.Context, j core.Job) error {
		return context.DeadlineExceeded
	})
	add("postponed", core.Job{Kind: core.JobReview, ProjectPath: "group/api", MRIID: 8, Username: "bob"}, func(ctx context.Context, j core.Job) error {
		return &core.RetryLater{At: now.Add(time.Hour), Reason: "limit"}
	})
	if o.Reader == nil {
		o.Reader = f.reader
	}
	o.Now = func() time.Time { return now }
	o.Logger = slog.New(slog.DiscardHandler)
	f.h = New(o)
	return f
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

func listData(t *testing.T, h http.Handler, q string) ListData {
	t.Helper()
	rec := get(h, DataPath+q, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", q, rec.Code, rec.Body)
	}
	var d ListData
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestListAndFilters(t *testing.T) {
	f := newFixture(t, Options{Enabled: true})
	d := listData(t, f.h, "")
	if len(d.Runs) != 3 || d.Runs[0].ID != f.ids["postponed"] || d.Runs[2].ID != f.ids["ok"] || d.Retention != "48h" {
		t.Fatalf("list: %+v", d)
	}
	ok := d.Runs[2]
	if ok.Status != "ok" || ok.ClaudeRuns != 1 || ok.Tokens != 12 || ok.Username != "alice" || ok.MRURL != "https://gitlab.example.com/group/api/-/merge_requests/7" {
		t.Fatalf("ok row: %+v", ok)
	}
	for q, want := range map[string]string{
		"?status=failed": f.ids["failed"], "?kind=chat": f.ids["ok"], "?project=WEB": f.ids["failed"], "?mr=!8": f.ids["postponed"],
	} {
		d := listData(t, f.h, q)
		if len(d.Runs) != 1 || d.Runs[0].ID != want {
			t.Errorf("%s: %+v", q, d.Runs)
		}
	}
	if d := listData(t, f.h, "?since=90m"); len(d.Runs) != 1 || d.Filter.Since != "1h30m0s" {
		t.Errorf("since: %+v", d)
	}
	if d := listData(t, f.h, "?project=group/api&status=ok"); len(d.Runs) != 1 {
		t.Errorf("combined: %+v", d.Runs)
	}
	for _, bad := range []string{"?status=weird", "?mr=x", "?since=1y", "?since=-1h"} {
		if rec := get(f.h, DataPath+bad, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
}

func TestPagesRender(t *testing.T) {
	f := newFixture(t, Options{Enabled: true})
	for _, p := range []string{ListPath, "/runs/", "/runs/" + f.ids["ok"]} {
		rec := get(f.h, p, nil)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d", p, rec.Code)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		m := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(csp)
		if m == nil || !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'nonce-") || !strings.Contains(csp, "'self'") {
			t.Fatalf("csp: %q", csp)
		}
		// Every script and style carries this response's nonce.
		for _, tag := range regexp.MustCompile(`<(script|style)\b[^>]*>`).FindAllString(body, -1) {
			if !strings.Contains(tag, `nonce="`+m[1]+`"`) {
				t.Errorf("%s: tag without nonce: %s", p, tag)
			}
		}
		// Self-contained: no placeholders, no data, no external resources.
		for _, s := range []string{"{{", "SECRET PROMPT", "alice", "<link ", "@import", `src="http`, `href="http`, "url(http"} {
			if strings.Contains(body, s) {
				t.Errorf("%s contains %q", p, s)
			}
		}
		if !strings.Contains(body, `href="/usage"`) {
			t.Errorf("%s: no link to /usage", p)
		}
	}
	// The hourly chart became a time ribbon drawn from data.json (no ECharts);
	// both pages load the shared run-detail renderer after common.js.
	list := get(f.h, ListPath, nil).Body.String()
	if strings.Contains(list, webassets.EChartsPath) || !strings.Contains(list, `id="tl"`) || !strings.Contains(list, `id="drawer"`) {
		t.Fatal("list page: time ribbon or side panel missing")
	}
	for _, p := range []string{ListPath, "/runs/" + f.ids["ok"]} {
		body := get(f.h, p, nil).Body.String()
		common, detail := strings.Index(body, "var D = (function"), strings.Index(body, `src="`+webassets.RunDetailPath+`"`)
		if common < 0 || detail < common || !strings.Contains(body, ".rd-wft") {
			t.Errorf("%s: run-detail renderer or its CSS missing (common.js at %d, renderer at %d)", p, common, detail)
		}
	}
	// Detail data.
	rec := get(f.h, "/runs/"+f.ids["ok"]+"/data.json", nil)
	var d RunData
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || d.Run == nil || len(d.Run.Claude) != 1 || d.Run.Claude[0].Prompt != "SECRET PROMPT <b>bold</b>" || len(d.Sizes) != 1 {
		t.Fatalf("run data: %v %+v", err, d)
	}
	if esc := "\\u003cb\\u003ebold"; !strings.Contains(rec.Body.String(), esc) || strings.Contains(rec.Body.String(), "<b>") {
		t.Error("JSON not HTML-escaped")
	}
	if rec := get(f.h, "/runs/20200101-000000-00000000", nil); rec.Code != http.StatusOK {
		t.Errorf("unknown id page: %d (page loads, data 404s)", rec.Code)
	}
	if rec := get(f.h, "/runs/20200101-000000-00000000/data.json", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id data: %d", rec.Code)
	}
	for _, p := range []string{"/runs/../etc", "/runs/x", "/runs/" + f.ids["ok"] + "/run.json", "/runs/" + f.ids["ok"] + "/claude-0.jsonl",
		"/runs/" + f.ids["ok"] + "/claude-01.jsonl", "/runs/" + f.ids["ok"] + "/a/b"} {
		if rec := get(f.h, p, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, DataPath, nil)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestRawDownload(t *testing.T) {
	f := newFixture(t, Options{Enabled: true})
	row, _, _ := f.reader.Get(context.Background(), f.ids["ok"])
	if err := os.WriteFile(f.reader.TranscriptPath(row, 1), []byte(`{"type":"system"}`+"\n"+`{"type":"result"}`+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	rec := get(f.h, "/runs/"+f.ids["ok"]+"/claude-1.jsonl", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "application/x-ndjson") || !strings.Contains(rec.Body.String(), `{"type":"result"}`) ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	if rec := get(f.h, "/runs/"+f.ids["ok"]+"/claude-2.jsonl", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("missing transcript: %d", rec.Code)
	}
}

func TestAuth(t *testing.T) {
	// Open without a token.
	f := newFixture(t, Options{Enabled: true})
	if rec := get(f.h, DataPath, nil); rec.Code != http.StatusOK {
		t.Fatalf("open: %d", rec.Code)
	}
	// Token enforced on every endpoint.
	f = newFixture(t, Options{Enabled: true, Token: "s3cret"})
	id := f.ids["ok"]
	for _, p := range []string{ListPath, DataPath, "/runs/" + id, "/runs/" + id + "/data.json", "/runs/" + id + "/claude-1.jsonl"} {
		if rec := get(f.h, p, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without token: %d", p, rec.Code)
		}
		if rec := get(f.h, p, map[string]string{"Authorization": "Bearer nope"}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s wrong token: %d", p, rec.Code)
		}
	}
	if rec := get(f.h, DataPath, map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusOK {
		t.Fatalf("bearer: %d", rec.Code)
	}
	rec := get(f.h, "/runs/"+id+"?token=s3cret", nil)
	loc := rec.Header().Get("Location")
	cs := rec.Result().Cookies()
	if rec.Code != http.StatusSeeOther || strings.Contains(loc, "token") || len(cs) != 1 || !cs[0].HttpOnly || cs[0].Path != ListPath || strings.Contains(cs[0].Value, "s3cret") {
		t.Fatalf("login: %d %q %+v", rec.Code, loc, cs)
	}
	if rec := get(f.h, "/runs/"+id+"/data.json", nil, cs[0]); rec.Code != http.StatusOK {
		t.Fatalf("cookie: %d", rec.Code)
	}
	// Disabled -> 404 everywhere, even with the token.
	f = newFixture(t, Options{Enabled: false, Token: "s3cret"})
	for _, p := range []string{ListPath, DataPath, "/runs/" + f.ids["ok"]} {
		if rec := get(f.h, p, map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusNotFound {
			t.Errorf("disabled %s: %d", p, rec.Code)
		}
	}
	// Startup warning only when open.
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	Register(http.NewServeMux(), Options{Reader: f.reader, Enabled: true, Logger: log})
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "DIAG_TOKEN") {
		t.Fatalf("no warning: %s", buf.String())
	}
	buf.Reset()
	Register(http.NewServeMux(), Options{Reader: f.reader, Enabled: true, Token: "x", Logger: log})
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("warning with token: %s", buf.String())
	}
}

// Data is only ever inserted as text: no innerHTML/outerHTML/
// insertAdjacentHTML/document.write/eval in the page sources.
func TestNoHTMLInjectionSinks(t *testing.T) {
	sinks := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*"on`)
	_, commonJS := webui.Assets()
	for name, src := range map[string]string{"list.html": listHTML, "run.html": runHTML, "common.js": commonJS, "diag.css": diagCSS} {
		if m := sinks.FindString(src); m != "" {
			t.Errorf("%s uses %s", name, m)
		}
		if strings.Contains(src, ` style="`) {
			t.Errorf("%s has an inline style attribute (blocked by the CSP)", name)
		}
	}
	// Links built from data go through safeHref (the run page renders with
	// rundetail.js, checked in internal/webassets).
	if !strings.Contains(listHTML, "D.safeHref(r.mr_url)") || !strings.Contains(runHTML, `src="{{RUNDETAIL}}"`) {
		t.Error("MR links not filtered by safeHref")
	}
}

func TestSafeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://g.example/x": "https://g.example/x", "javascript:alert(1)": "", "//evil": "", "": "", "http://h/p": "http://h/p",
	} {
		if got := safeURL(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

// The kind filter offers every core.JobKind (pause, resume, ignore,
// resolve_all, config, memory, ...) with a label, generated from one list.
func TestKindFilterHasEveryJobKind(t *testing.T) {
	f := newFixture(t, Options{Enabled: true})
	body := get(f.h, ListPath, nil).Body.String()
	sel := regexp.MustCompile(`(?s)<select id="f-kind"[^>]*>(.*?)</select>`).FindStringSubmatch(body)
	if sel == nil {
		t.Fatal("kind filter not found")
	}
	for _, k := range core.AllJobKinds() {
		if info, _ := k.Info(); info.Label == "" {
			t.Errorf("no label for kind %s", k)
		}
		opt := `<option value="` + string(k) + `">` + kindLabel(k) + `</option>`
		if !strings.Contains(sel[1], opt) {
			t.Errorf("filter lacks %s", opt)
		}
		if !strings.Contains(body, `"`+string(k)+`":"`+kindLabel(k)+`"`) {
			t.Errorf("JS labels lack %s", k)
		}
	}
	for _, k := range []string{"pause", "resume", "ignore", "resolve_all", "config", "memory"} {
		if !strings.Contains(sel[1], `value="`+k+`"`) {
			t.Errorf("filter lacks %s", k)
		}
	}
}

// Every page of the bot links to the other three.
func TestPagesLinkToOtherPages(t *testing.T) {
	f := newFixture(t, Options{Enabled: true})
	for _, p := range []string{ListPath, "/runs/" + f.ids["ok"]} {
		body := get(f.h, p, nil).Body.String()
		for _, l := range []string{"/usage", "/map", "/problems"} {
			if !strings.Contains(body, `href="`+l+`"`) {
				t.Errorf("%s does not link to %s", p, l)
			}
		}
	}
}

// The /runs filter row is a filter bar over native controls with the same
// URL parameters as before; no page has inline styles or handlers.
func TestFilterBarAndNoInlineMarkup(t *testing.T) {
	f := newFixture(t, Options{Enabled: true})
	list := get(f.h, ListPath, nil).Body.String()
	for _, want := range []string{
		`src="` + webassets.FiltersPath + `"`, `id="filters" data-review-filters`, `name="status"`, `name="kind"`, `name="project"`, `name="mr"`,
		`data-prefix="!"`, `D.filters.enhance($("filters"), { onChange: applyFilters })`,
	} {
		if !strings.Contains(list, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	for _, bad := range []string{`id="f-reset"`, `$("f-reset")`, ">Применить<", "setSelect", "initCombos"} {
		if strings.Contains(list, bad) {
			t.Errorf("list still has %q", bad)
		}
	}
	for _, p := range []string{ListPath, "/runs/" + f.ids["ok"]} {
		if v := webui.InlineViolations(get(f.h, p, nil).Body.String()); len(v) != 0 {
			t.Errorf("%s: inline style/handler (blocked by the CSP): %q", p, v)
		}
	}
}
