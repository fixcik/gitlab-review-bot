package problemsweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

type fakeStore struct {
	mu      sync.Mutex
	rows    []problems.Record
	acked   []string
	unacked []string
	last    problems.Filter
	listErr error // ListProblems fails
	ackErr  error // AckProblem / UnackProblem fail
}

func (f *fakeStore) ListProblems(_ context.Context, flt problems.Filter) ([]problems.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if flt.Limit != 5000 {
		f.last = flt
	}
	var out []problems.Record
	for _, r := range f.rows {
		if flt.Kind != "" && r.Kind != flt.Kind {
			continue
		}
		if flt.Project != "" && !strings.EqualFold(r.ProjectPath, flt.Project) {
			continue
		}
		if (flt.Status == problems.StatusNew && (!r.AckedAt.IsZero() || !r.ResolvedAt.IsZero())) ||
			(flt.Status == problems.StatusAcked && r.AckedAt.IsZero()) ||
			(flt.Status == problems.StatusResolved && r.ResolvedAt.IsZero()) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeStore) AckProblem(_ context.Context, fp string, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ackErr != nil {
		return false, f.ackErr
	}
	f.acked = append(f.acked, fp)
	for i := range f.rows {
		if f.rows[i].Fingerprint == fp && f.rows[i].AckedAt.IsZero() {
			f.rows[i].AckedAt = at
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) UnackProblem(_ context.Context, fp string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ackErr != nil {
		return false, f.ackErr
	}
	f.unacked = append(f.unacked, fp)
	for i := range f.rows {
		if f.rows[i].Fingerprint == fp && !f.rows[i].AckedAt.IsZero() {
			f.rows[i].AckedAt = time.Time{}
			return true, nil
		}
	}
	return false, nil
}

const token = "page-token-123456"

var fpA = problems.Fingerprint(problems.ModelOther, "group/api", "x")

func testHandler(t *testing.T, tok string) (http.Handler, *fakeStore) {
	t.Helper()
	at := time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)
	st := &fakeStore{rows: []problems.Record{
		{Fingerprint: fpA, Kind: problems.ModelOther, Source: problems.SourceModel, ProjectPath: "group/api",
			Subject: `<script>alert("s")</script>`, Detail: `"><img src=x onerror=alert(1)>`, FirstSeen: at, LastSeen: at, Count: 3,
			LastMRIID: 12, LastRunID: "20261026-093000-abcd1234"},
		{Fingerprint: problems.Fingerprint(problems.LintFailed, "group/web", "eslint"), Kind: problems.LintFailed, Source: problems.SourceBot,
			ProjectPath: "group/web", Subject: "eslint: недоступен", FirstSeen: at, LastSeen: at, Count: 1, LastRunID: "../../etc",
			AckedAt: at},
		{Fingerprint: problems.Fingerprint(problems.BaseUnavailable, "evil", "x"), Kind: problems.BaseUnavailable, ProjectPath: "javascript:alert(1)//x",
			Subject: "setup", LastMRIID: 3, FirstSeen: at, LastSeen: at},
	}}
	h := New(Options{Store: st, Enabled: true, Token: tok, GitLabURL: "https://gitlab.example.com/", Location: time.UTC,
		Now: func() time.Time { return at.Add(time.Hour) }})
	return h, st
}

func do(h http.Handler, method, target string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]{64})"`)

func TestPageAuthAndRender(t *testing.T) {
	h, st := testHandler(t, token)
	if w := do(h, "GET", "/problems", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	// ?token= login: cookie + redirect without the token.
	w := do(h, "GET", "/problems?token="+token+"&kind=lint_failed", nil, "")
	if w.Code != http.StatusSeeOther || strings.Contains(w.Header().Get("Location"), token) {
		t.Fatalf("login: %d %v", w.Code, w.Header())
	}
	cookie := w.Result().Cookies()[0]
	w = do(h, "GET", "/problems", map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("cookie: %d", w.Code)
	}
	body := w.Body.String()
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe") {
		t.Errorf("csp %q", csp)
	}
	if w.Header().Get("Referrer-Policy") != "same-origin" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers %v", w.Header())
	}
	// Escaping: no markup from the data survives.
	for _, bad := range []string{"<script>alert", "<img src=x", `"><img`} {
		if strings.Contains(body, bad) {
			t.Errorf("unescaped %q", bad)
		}
	}
	for _, want := range []string{"&lt;script&gt;alert(&#34;s&#34;)&lt;/script&gt;", "Модель: не удалось проверить", "сообщила модель",
		`href="https://gitlab.example.com/group/api/-/merge_requests/12"`, `href="/runs/20261026-093000-abcd1234"`, "26.10.2026 09:30",
		"Как исправить"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Default status: active only (the acked row is hidden); unsafe
	// project paths and run ids give no links.
	if strings.Contains(body, "eslint") || strings.Contains(body, "/runs/../") || strings.Contains(body, "javascript:alert(1)//x/-/") {
		t.Error("acked row or unsafe link shown")
	}
	if st.last.Status != problems.StatusNew {
		t.Errorf("default filter %+v", st.last)
	}
	// Filters.
	b := do(h, "GET", "/problems?status=all&kind=lint_failed&project=group/web", map[string]string{"Authorization": "Bearer " + token}, "").Body.String()
	if st.last.Status != "" || st.last.Kind != problems.LintFailed || st.last.Project != "group/web" || !strings.Contains(b, "eslint") || !strings.Contains(b, "скрыта") {
		t.Errorf("filters %+v", st.last)
	}
	do(h, "GET", "/problems?status=bogus&kind=bogus", map[string]string{"Authorization": "Bearer " + token}, "")
	if st.last.Status != problems.StatusNew || st.last.Kind != "" {
		t.Errorf("bad filters must fall back: %+v", st.last)
	}
	if w := do(h, "HEAD", "/problems", map[string]string{"Authorization": "Bearer " + token}, ""); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD %d %d", w.Code, w.Body.Len())
	}
	if w := do(h, "GET", "/problems", map[string]string{"Authorization": "Bearer wrong"}, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer %d", w.Code)
	}
}

func TestAckProtections(t *testing.T) {
	h, st := testHandler(t, token)
	auth := map[string]string{"Authorization": "Bearer " + token}
	page := do(h, "GET", "/problems", auth, "").Body.String()
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token in the page")
	}
	csrf := m[1]
	form := func(fp, csrf string) string {
		return url.Values{"fingerprint": {fp}, "csrf": {csrf}, "back": {"status=all&kind=model_other&evil=1"}}.Encode()
	}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{"Authorization": "Bearer " + token}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for name, tc := range map[string]struct {
		hdr  map[string]string
		body string
		code int
	}{
		"no auth":           {map[string]string{"Origin": "http://example.com"}, form(fpA, csrf), http.StatusUnauthorized},
		"no origin":         {with(nil), form(fpA, csrf), http.StatusForbidden},
		"null origin":       {with(map[string]string{"Origin": "null"}), form(fpA, csrf), http.StatusForbidden},
		"foreign origin":    {with(map[string]string{"Origin": "https://evil.example"}), form(fpA, csrf), http.StatusForbidden},
		"foreign referer":   {with(map[string]string{"Referer": "https://evil.example/problems"}), form(fpA, csrf), http.StatusForbidden},
		"bad csrf":          {with(map[string]string{"Origin": "http://example.com"}), form(fpA, strings.Repeat("0", 64)), http.StatusForbidden},
		"no csrf":           {with(map[string]string{"Origin": "http://example.com"}), form(fpA, ""), http.StatusForbidden},
		"bad fingerprint":   {with(map[string]string{"Origin": "http://example.com"}), form("../x", csrf), http.StatusBadRequest},
		"origin javascript": {with(map[string]string{"Origin": "javascript://example.com"}), form(fpA, csrf), http.StatusForbidden},
	} {
		if w := do(h, "POST", "/problems/ack", tc.hdr, tc.body); w.Code != tc.code {
			t.Errorf("%s: %d, want %d (%s)", name, w.Code, tc.code, w.Body.String())
		}
	}
	if len(st.acked) != 0 {
		t.Fatalf("acked despite protections: %v", st.acked)
	}
	// Same origin (httptest host is example.com) via Referer, valid token.
	w := do(h, "POST", "/problems/ack", with(map[string]string{"Referer": "http://example.com/problems?status=all"}), form(fpA, csrf))
	// The redirect keeps the page filters of "back" (not "evil") and names
	// what was hidden for the banner's "Вернуть".
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/problems?hidden="+fpA+"&kind=model_other&status=all" {
		t.Fatalf("ack: %d %q", w.Code, w.Header().Get("Location"))
	}
	if len(st.acked) != 1 || st.acked[0] != fpA {
		t.Fatalf("acked %v", st.acked)
	}
	// A token from another handler (another process / access token) fails.
	other, _ := testHandler(t, "another-token-99999")
	m2 := csrfRe.FindStringSubmatch(do(other, "GET", "/problems", map[string]string{"Authorization": "Bearer another-token-99999"}, "").Body.String())
	if m2 == nil || m2[1] == csrf {
		t.Fatal("csrf tokens must differ per process and token")
	}
	if w := do(h, "POST", "/problems/ack", with(map[string]string{"Origin": "http://example.com"}), form(fpA, m2[1])); w.Code != http.StatusForbidden {
		t.Errorf("foreign csrf: %d", w.Code)
	}
}

func TestMethodsAndDisabled(t *testing.T) {
	h, _ := testHandler(t, "")
	for _, tc := range []struct{ method, path, allow string }{
		{"POST", "/problems", "GET, HEAD"},
		{"PUT", "/problems", "GET, HEAD"},
		{"DELETE", "/problems", "GET, HEAD"},
		{"GET", "/problems/ack", "POST"},
		{"PUT", "/problems/ack", "POST"},
	} {
		w := do(h, tc.method, tc.path, nil, "")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s: %d allow %q", tc.method, tc.path, w.Code, w.Header().Get("Allow"))
		}
	}
	if w := do(h, "GET", "/problems/other", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown path %d", w.Code)
	}
	// Open page (no token): still same-origin + CSRF on the POST.
	page := do(h, "GET", "/problems", nil, "").Body.String()
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf in the open page")
	}
	if w := do(h, "POST", "/problems/ack", nil, url.Values{"fingerprint": {fpA}, "csrf": {m[1]}}.Encode()); w.Code != http.StatusForbidden {
		t.Errorf("open page, no origin: %d", w.Code)
	}
	if w := do(h, "POST", "/problems/ack", map[string]string{"Origin": "http://example.com"}, url.Values{"fingerprint": {fpA}, "csrf": {m[1]}}.Encode()); w.Code != http.StatusSeeOther {
		t.Errorf("open page, same origin: %d", w.Code)
	}
	off := New(Options{Store: &fakeStore{}, Enabled: false})
	if w := do(off, "GET", "/problems", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("disabled: %d", w.Code)
	}
	if w := do(New(Options{Enabled: true}), "GET", "/problems", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("no store: %d", w.Code)
	}
}

func TestRegisterMounts(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, Options{Store: &fakeStore{}, Enabled: true})
	for _, p := range []string{"/problems", "/problems/ack"} {
		if _, pat := mux.Handler(httptest.NewRequest("GET", p, nil)); pat == "" {
			t.Errorf("%s not mounted", p)
		}
	}
}

// Every page of the bot links to the other three.
func TestPageLinksToOtherPages(t *testing.T) {
	h, _ := testHandler(t, "")
	body := do(h, "GET", "/problems", nil, "").Body.String()
	for _, l := range []string{"/runs", "/usage", "/map"} {
		if !strings.Contains(body, `href="`+l+`"`) {
			t.Errorf("the problems page does not link to %s", l)
		}
	}
}

// A problem the bot found fixed is "исправлена": not among the active ones,
// listed under its own filter, without the hide button.
func TestResolvedProblemShownAsFixed(t *testing.T) {
	h, st := testHandler(t, "")
	st.mu.Lock()
	st.rows[0].ResolvedAt = time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	st.mu.Unlock()
	active := do(h, "GET", "/problems", nil, "").Body.String()
	if strings.Contains(active, "исправлена</span>") || strings.Contains(active, fpA) {
		t.Error("a resolved problem is listed as active")
	}
	fixed := do(h, "GET", "/problems?status=resolved", nil, "").Body.String()
	if !strings.Contains(fixed, "исправлена</span>") || strings.Contains(fixed, "активна</span>") {
		t.Error("the resolved filter does not show the fixed problem")
	}
	if strings.Contains(fixed, `value="`+fpA+`"`) {
		t.Error("a resolved problem has the hide button")
	}
	if all := do(h, "GET", "/problems?status=all", nil, "").Body.String(); !strings.Contains(all, "исправлена</span>") || !strings.Contains(all, "активна</span>") {
		t.Error("the all filter lacks a status")
	}
}

// The filter row is a GET form enhanced by the filter bar: the native
// selects stay the source of truth (no JS = plain form with its button),
// the status default comes from the server, nothing is inline.
func TestFilterBar(t *testing.T) {
	h, _ := testHandler(t, token)
	auth := map[string]string{"Authorization": "Bearer " + token}
	body := do(h, "GET", "/problems?status=acked&project=group/web", auth, "").Body.String()
	for _, want := range []string{
		`<form class="filters" method="get" action="/problems" role="search" aria-label="Фильтры" data-review-filters>`,
		`<select name="status" data-label="Статус" data-default="new">`, `<option value="acked" selected>скрытые</option>`,
		`<select name="kind" data-label="Вид" data-type="search-select">`, `<select name="project" data-label="Проект" data-type="search-select">`,
		`<option value="">все</option>`, `<button type="submit" class="btn sm" data-rf-fallback>Применить</button>`,
		`src="` + webassets.FiltersPath + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if v := webui.InlineViolations(body); len(v) != 0 {
		t.Errorf("inline style/handler (blocked by the CSP): %q", v)
	}
	for _, bad := range []string{"TomSelect", "tom-select", "initCombos", "{{"} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
}
