package knowledgeweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

type fakeStore struct {
	mu        sync.Mutex
	rules     []learn.Rule // live
	dead      []learn.Rule // deleted
	mem       []memory.Section
	stats     map[string]learn.OutcomeStats
	decisions []learn.Outcome
	series    []learn.Outcome
	conflicts map[int64]int64 // UndeleteLearnings: deleted id -> live id
	since     time.Time
	filter    learn.DecisionFilter
	seriesArg struct {
		project string
		limit   int
	}
	deleted  []int64
	restored []int64
	scoped   map[int64]string
	failList bool
	failMut  bool
}

var errBoom = errors.New("boom")

func (f *fakeStore) ListAllLearnings(context.Context, int) ([]learn.Rule, error) {
	if f.failList {
		return nil, errBoom
	}
	return f.rules, nil
}

func (f *fakeStore) ListDeletedLearnings(context.Context, int) ([]learn.Rule, error) {
	return f.dead, nil
}

func (f *fakeStore) live(id int64) *learn.Rule {
	for i := range f.rules {
		if f.rules[i].ID == id {
			return &f.rules[i]
		}
	}
	return nil
}

// The fake is all or nothing like the store: every id is checked first.
func (f *fakeStore) DeleteLearnings(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failMut {
		return errBoom
	}
	for _, id := range ids {
		if f.live(id) == nil {
			return fmt.Errorf("fake %d: %w", id, learn.ErrRuleNotFound)
		}
	}
	f.deleted = append(f.deleted, ids...)
	return nil
}

func (f *fakeStore) SetLearningsScope(_ context.Context, ids []int64, scope string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failMut {
		return errBoom
	}
	for _, id := range ids {
		r := f.live(id)
		if r == nil {
			return fmt.Errorf("fake %d: %w", id, learn.ErrRuleNotFound)
		}
		if scope == learn.ScopeProject && r.ProjectPath == "" {
			return fmt.Errorf("fake %d: %w", id, learn.ErrRuleNoProject)
		}
	}
	if f.scoped == nil {
		f.scoped = map[int64]string{}
	}
	for _, id := range ids {
		f.scoped[id] = scope
	}
	return nil
}

func (f *fakeStore) UndeleteLearnings(_ context.Context, ids []int64) (map[int64]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failMut {
		return nil, errBoom
	}
	conflicts := map[int64]int64{}
	for _, id := range ids {
		found := false
		for _, r := range f.dead {
			found = found || r.ID == id
		}
		if !found {
			return nil, fmt.Errorf("fake %d: %w", id, learn.ErrRuleNotFound)
		}
		if live, ok := f.conflicts[id]; ok {
			conflicts[id] = live
		}
	}
	if len(conflicts) > 0 {
		return conflicts, nil
	}
	f.restored = append(f.restored, ids...)
	return nil, nil
}

func (f *fakeStore) ListAllMemory(context.Context) ([]memory.Section, error) { return f.mem, nil }

func (f *fakeStore) OutcomeStatsAll(_ context.Context, since time.Time) (map[string]learn.OutcomeStats, error) {
	f.since = since
	return f.stats, nil
}

func (f *fakeStore) ListDecisions(_ context.Context, flt learn.DecisionFilter) ([]learn.Outcome, error) {
	f.filter = flt
	var out []learn.Outcome
	for _, o := range f.decisions {
		if flt.Status == "" || o.Status == flt.Status {
			out = append(out, o)
		}
	}
	if flt.Offset >= len(out) {
		return nil, nil
	}
	out = out[flt.Offset:]
	if flt.Limit > 0 && len(out) > flt.Limit {
		out = out[:flt.Limit]
	}
	return out, nil
}

func (f *fakeStore) DecisionSeries(_ context.Context, project string, _ time.Time, limit int) ([]learn.Outcome, error) {
	f.seriesArg.project, f.seriesArg.limit = project, limit
	return f.series, nil
}

const token = "page-token-123456"

var now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func testHandler(t *testing.T, tok string) (http.Handler, *fakeStore) {
	t.Helper()
	at := time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)
	st := &fakeStore{
		rules: []learn.Rule{
			{ID: 1, Scope: learn.ScopeGlobal, ProjectPath: "g/api", Text: `<b>Всегда</b> проверяй ошибки`, Author: "rb-dev",
				SourceURL: "https://gitlab.example.com/g/api/-/merge_requests/3#note_5", CreatedAt: at, UpdatedAt: at},
			{ID: 2, Scope: learn.ScopeProject, ProjectPath: "g/web", Text: "Не трогай vendor", Author: "bob",
				SourceURL: "javascript:alert(1)", CreatedAt: at, UpdatedAt: at},
			{ID: 3, Scope: learn.ScopeGlobal, Text: "Без проекта", SourceURL: "https://evil.example/x", CreatedAt: at, UpdatedAt: at},
		},
		mem: []memory.Section{
			{ProjectID: 1, ProjectPath: "g/api", Key: memory.SectionBuild, Content: "- `make build`\n- <i>x</i>\n\n", UpdatedAt: at, SourceNote: "ревью !7"},
			{ProjectID: 1, ProjectPath: "g/api", Key: memory.SectionTest, Content: "- `make test`", UpdatedAt: at},
			{ProjectID: 2, ProjectPath: "g/web", Key: "unknown", Content: "строка без маркера", UpdatedAt: at},
		},
		stats: map[string]learn.OutcomeStats{
			"g/api": {Fixed: 3, Dismissed: 1, Open: 2},
			"g/web": {ResolvedByBot: 4},
		},
		dead: []learn.Rule{
			{ID: 7, Scope: learn.ScopeProject, ProjectPath: "g/api", Text: "Старое правило", CreatedAt: at, UpdatedAt: at, DeletedAt: at.Add(time.Hour)},
		},
		decisions: []learn.Outcome{
			{ID: 41, ProjectPath: "g/api", MRIID: 12, Title: `</script><img src=x onerror=alert(1)>`, Severity: "critical", Status: learn.OutcomeDismissed,
				Path: "a.go", Line: 7, BodyExcerpt: "Используйте `errors.Is` <b>тут</b>", RuleID: 7, UpdatedAt: at},
			{ID: 42, ProjectPath: "g/api", MRIID: 12, Title: "Утечка", Severity: `x" onclick="1`, Status: learn.OutcomeFixed, Path: "b.go", UpdatedAt: at},
			{ID: 43, ProjectPath: "../evil", MRIID: 3, Status: learn.OutcomeRejectedByVerifier, RuleID: 1, UpdatedAt: at},
		},
		series: []learn.Outcome{
			{ProjectPath: "g/api", MRIID: 12, Title: "a", Status: learn.OutcomeFixed, UpdatedAt: at},
			{ProjectPath: "g/api", MRIID: 12, Title: `</script><b>x`, Status: learn.OutcomeDismissed, UpdatedAt: at},
			{ProjectPath: "g/api", MRIID: 12, Status: learn.OutcomeFixed, UpdatedAt: at},
			{ProjectPath: "g/api", MRIID: 13, Title: "d", Status: learn.OutcomeFixed, UpdatedAt: at},
		},
	}
	h := New(Options{Store: st, Enabled: true, Token: tok, GitLabURL: "https://gitlab.example.com/", Location: time.UTC,
		Now: func() time.Time { return now }})
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
	if w := do(h, "GET", "/knowledge", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	w := do(h, "GET", "/knowledge?token="+token, nil, "")
	if w.Code != http.StatusSeeOther || strings.Contains(w.Header().Get("Location"), token) {
		t.Fatalf("login: %d %v", w.Code, w.Header())
	}
	cookie := w.Result().Cookies()[0]
	w = do(h, "GET", "/knowledge", map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}, "")
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
	for _, bad := range []string{"<b>Всегда", "<i>x</i>", "javascript:alert", "evil.example"} {
		if strings.Contains(body, bad) {
			t.Errorf("unsafe %q", bad)
		}
	}
	for _, want := range []string{"&lt;b&gt;Всегда&lt;/b&gt; проверяй ошибки", `href="https://gitlab.example.com/g/api/-/merge_requests/3#note_5"`,
		"@rb-dev", "26.10.2026 09:30", "Сборка", "Тесты", "unknown", "строка без маркера", "<li>`make build`</li>", "ревью !7",
		"75%", "принято 3 из 4 решений", "мало решений (4 &lt; 10)", "Открыть карту", `action="/knowledge/rule/delete"`, "Только для проекта", "Сделать общим"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The rule without a project cannot be limited to one; the project rule can be made global.
	if strings.Count(body, ">Только для проекта</button>") != 1 || strings.Count(body, ">Сделать общим</button>") != 1 {
		t.Error("scope buttons")
	}
	// Default period is 30 days.
	if want := now.AddDate(0, 0, -30); !st.since.Equal(want) {
		t.Errorf("since %v, want %v", st.since, want)
	}
	if w := do(h, "HEAD", "/knowledge", map[string]string{"Authorization": "Bearer " + token}, ""); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD %d %d", w.Code, w.Body.Len())
	}
	if w := do(h, "POST", "/knowledge", map[string]string{"Authorization": "Bearer " + token}, ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST page %d", w.Code)
	}
	if w := do(h, "GET", "/knowledge/rule/delete", map[string]string{"Authorization": "Bearer " + token}, ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET action %d", w.Code)
	}
	if w := do(h, "GET", "/knowledge/other", map[string]string{"Authorization": "Bearer " + token}, ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown path %d", w.Code)
	}
}

func TestFilters(t *testing.T) {
	h, st := testHandler(t, "")
	b := do(h, "GET", "/knowledge?project=G/WEB&period=all", nil, "").Body.String()
	// The project's own rule and the global ones stay; memory and accuracy of other projects go.
	for _, want := range []string{"Не трогай vendor", "Без проекта", "Всегда"} {
		if !strings.Contains(b, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(b, "make build") || !strings.Contains(b, "строка без маркера") {
		t.Error("memory filter")
	}
	if !st.since.IsZero() {
		t.Errorf("all time: since %v", st.since)
	}
	if !strings.Contains(b, `<option value="all" selected>`) {
		t.Error("period not selected")
	}
	do(h, "GET", "/knowledge?period=bogus", nil, "")
	if want := now.AddDate(0, 0, -30); !st.since.Equal(want) {
		t.Errorf("bad period must fall back to 30 days: %v", st.since)
	}
	do(h, "GET", "/knowledge?period=7", nil, "")
	if want := now.AddDate(0, 0, -7); !st.since.Equal(want) {
		t.Errorf("7 days: %v", st.since)
	}
}

func TestEmptyAndErrors(t *testing.T) {
	st := &fakeStore{}
	h := New(Options{Store: st, Enabled: true, Location: time.UTC})
	b := do(h, "GET", "/knowledge", nil, "").Body.String()
	for _, want := range []string{"Правил и памяти пока нет", "замечаний не было", "решений команды пока нет", "Нет решений по замечаниям"} {
		if !strings.Contains(b, want) {
			t.Errorf("lacks %q", want)
		}
	}
	st.failList = true
	if b := do(h, "GET", "/knowledge", nil, "").Body.String(); !strings.Contains(b, "Не удалось прочитать") {
		t.Error("read error not shown")
	}
	for _, o := range []Options{{Store: st}, {Enabled: true}} {
		if w := do(New(o), "GET", "/knowledge", nil, ""); w.Code != http.StatusNotFound {
			t.Errorf("disabled: %d", w.Code)
		}
	}
	// Truncation marker.
	st = &fakeStore{}
	for i := 0; i <= maxRules; i++ {
		st.rules = append(st.rules, learn.Rule{ID: int64(i + 1), Scope: learn.ScopeGlobal, Text: "r"})
	}
	if b := do(New(Options{Store: st, Enabled: true}), "GET", "/knowledge", nil, "").Body.String(); !strings.Contains(b, "1000+") {
		t.Error("no truncation marker")
	}
}

func TestRegisterLogs(t *testing.T) {
	for _, o := range []Options{{}, {Enabled: true, Store: &fakeStore{}}, {Enabled: true, Store: &fakeStore{}, Token: "t"}} {
		mux := http.NewServeMux()
		Register(mux, o)
		Register(http.NewServeMux(), Options{Store: o.Store, Enabled: o.Enabled, Token: o.Token, Logger: nil})
	}
}

func TestActions(t *testing.T) {
	h, st := testHandler(t, token)
	auth := map[string]string{"Authorization": "Bearer " + token}
	m := csrfRe.FindStringSubmatch(do(h, "GET", "/knowledge", auth, "").Body.String())
	if m == nil {
		t.Fatal("no csrf token in the page")
	}
	csrf := m[1]
	hdr := func(extra map[string]string) map[string]string {
		out := map[string]string{"Authorization": "Bearer " + token}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	ok := map[string]string{"Origin": "http://example.com"}
	form := func(kv ...string) string {
		v := url.Values{"csrf": {csrf}, "back": {"project=g/web&period=7&kb=deleted&evil=1"}}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v.Encode()
	}
	many := func(n int, path string, extra ...string) string {
		v := url.Values{"csrf": {csrf}}
		for i := 0; i < n; i++ {
			v.Add("id", strconv.Itoa(i+1))
		}
		for i := 0; i < len(extra); i += 2 {
			v.Set(extra[i], extra[i+1])
		}
		return v.Encode()
	}
	refusals := map[string]struct {
		hdr  map[string]string
		path string
		body string
		code int
	}{
		"bad id in a batch": {hdr(ok), "/knowledge/rule/delete", "csrf=" + csrf + "&id=1&id=2x", http.StatusBadRequest},
		"negative id":       {hdr(ok), "/knowledge/rule/delete", form("id", "-1"), http.StatusBadRequest},
		"id with a plus":    {hdr(ok), "/knowledge/rule/delete", form("id", "+1"), http.StatusBadRequest},
		"huge id":           {hdr(ok), "/knowledge/rule/delete", form("id", "99999999999999999999"), http.StatusBadRequest},
		"no id":             {hdr(ok), "/knowledge/rule/delete", form(), http.StatusBadRequest},
		"too many":          {hdr(ok), "/knowledge/rule/delete", many(maxBulk+1, ""), http.StatusBadRequest},
		"too many scope":    {hdr(ok), "/knowledge/rule/scope", many(maxBulk+1, "", "scope", "global"), http.StatusBadRequest},
		"too many restore":  {hdr(ok), "/knowledge/rule/restore", many(maxBulk+1, ""), http.StatusBadRequest},
		"body too big":      {hdr(ok), "/knowledge/rule/delete", form("id", "1", "pad", strings.Repeat("x", maxBodySize)), http.StatusBadRequest},
		"unknown in batch":  {hdr(ok), "/knowledge/rule/delete", "csrf=" + csrf + "&id=1&id=99", http.StatusNotFound},
		"scope unknown":     {hdr(ok), "/knowledge/rule/scope", "csrf=" + csrf + "&scope=global&id=1&id=99", http.StatusNotFound},
		"scope no project":  {hdr(ok), "/knowledge/rule/scope", "csrf=" + csrf + "&scope=project&id=1&id=3", http.StatusBadRequest},
		"restore live rule": {hdr(ok), "/knowledge/rule/restore", form("id", "1"), http.StatusNotFound},
		"restore no csrf":   {hdr(ok), "/knowledge/rule/restore", form("id", "7", "csrf", ""), http.StatusForbidden},
		"restore no origin": {hdr(nil), "/knowledge/rule/restore", form("id", "7"), http.StatusForbidden},
		"restore foreign":   {hdr(map[string]string{"Origin": "https://evil.example"}), "/knowledge/rule/restore", form("id", "7"), http.StatusForbidden},
		"restore no auth":   {ok, "/knowledge/rule/restore", form("id", "7"), http.StatusUnauthorized},
	}
	for name, tc := range map[string]struct {
		hdr  map[string]string
		path string
		body string
		code int
	}{
		"no auth":         {map[string]string{"Origin": "http://example.com"}, "/knowledge/rule/delete", form("id", "1"), http.StatusUnauthorized},
		"no origin":       {hdr(nil), "/knowledge/rule/delete", form("id", "1"), http.StatusForbidden},
		"null origin":     {hdr(map[string]string{"Origin": "null"}), "/knowledge/rule/delete", form("id", "1"), http.StatusForbidden},
		"foreign origin":  {hdr(map[string]string{"Origin": "https://evil.example"}), "/knowledge/rule/delete", form("id", "1"), http.StatusForbidden},
		"foreign referer": {hdr(map[string]string{"Referer": "https://evil.example/x"}), "/knowledge/rule/delete", form("id", "1"), http.StatusForbidden},
		"bad csrf":        {hdr(ok), "/knowledge/rule/delete", form("id", "1", "csrf", strings.Repeat("0", 64)), http.StatusForbidden},
		"no csrf":         {hdr(ok), "/knowledge/rule/delete", form("id", "1", "csrf", ""), http.StatusForbidden},
		"bad id":          {hdr(ok), "/knowledge/rule/delete", form("id", "x"), http.StatusBadRequest},
		"zero id":         {hdr(ok), "/knowledge/rule/delete", form("id", "0"), http.StatusBadRequest},
		"bad scope":       {hdr(ok), "/knowledge/rule/scope", form("id", "1", "scope", "all"), http.StatusBadRequest},
		"unknown rule":    {hdr(ok), "/knowledge/rule/scope", form("id", "99", "scope", "global"), http.StatusNotFound},
		"no project":      {hdr(ok), "/knowledge/rule/scope", form("id", "3", "scope", "project"), http.StatusBadRequest},
	} {
		refusals[name] = tc
	}
	for name, tc := range refusals {
		if w := do(h, "POST", tc.path, tc.hdr, tc.body); w.Code != tc.code {
			t.Errorf("%s: %d, want %d (%s)", name, w.Code, tc.code, w.Body.String())
		}
	}
	if len(st.deleted) != 0 || len(st.scoped) != 0 || len(st.restored) != 0 {
		t.Fatalf("a refused request changed data: %v %v %v", st.deleted, st.scoped, st.restored)
	}
	// Every POST path answers 405 to other methods.
	for _, p := range []string{DeletePath, ScopePath, RestorePath} {
		if w := do(h, "GET", p, auth, ""); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" {
			t.Errorf("GET %s: %d", p, w.Code)
		}
	}

	// Delete several (duplicates dropped), then back to the filtered page
	// (only the known filters survive) with the result for the banner.
	w := do(h, "POST", "/knowledge/rule/delete", hdr(ok), "csrf="+csrf+"&id=2&id=1&id=2&back="+url.QueryEscape("project=g/web&period=7&evil=1"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/knowledge?deleted=2%2C1&period=7&project=g%2Fweb#kb" ||
		fmt.Sprint(st.deleted) != "[2 1]" {
		t.Fatalf("delete: %d %q %v", w.Code, w.Header().Get("Location"), st.deleted)
	}
	// The single-row form still works; the Referer is accepted instead of the Origin.
	ref := map[string]string{"Referer": "http://example.com/knowledge"}
	if w := do(h, "POST", "/knowledge/rule/scope", hdr(ref), form("id", "1", "scope", "project")); w.Code != http.StatusSeeOther || st.scoped[1] != learn.ScopeProject ||
		w.Header().Get("Location") != "/knowledge?kb=deleted&period=7&project=g%2Fweb&scope=project&scoped=1#kb" {
		t.Fatalf("limit: %d %q %v", w.Code, w.Header().Get("Location"), st.scoped)
	}
	if w := do(h, "POST", "/knowledge/rule/scope", hdr(ok), "csrf="+csrf+"&scope=global&id=2&id=3"); w.Code != http.StatusSeeOther ||
		st.scoped[2] != learn.ScopeGlobal || st.scoped[3] != learn.ScopeGlobal {
		t.Fatalf("global: %d %v", w.Code, st.scoped)
	}
	// Restore: done, or refused because an equal rule is live (nothing restored).
	if w := do(h, "POST", "/knowledge/rule/restore", hdr(ok), "csrf="+csrf+"&id=7"); w.Code != http.StatusSeeOther ||
		w.Header().Get("Location") != "/knowledge?restored=1#kb" || fmt.Sprint(st.restored) != "[7]" {
		t.Fatalf("restore: %d %q %v", w.Code, w.Header().Get("Location"), st.restored)
	}
	st.restored, st.conflicts = nil, map[int64]int64{7: 1}
	if w := do(h, "POST", "/knowledge/rule/restore", hdr(ok), "csrf="+csrf+"&id=7"); w.Code != http.StatusSeeOther ||
		w.Header().Get("Location") != "/knowledge?conflict=7&conflicts=1&live=1#kb" || len(st.restored) != 0 {
		t.Fatalf("conflict: %d %q %v", w.Code, w.Header().Get("Location"), st.restored)
	}
	st.conflicts = nil
	// Redirect without filters.
	if w := do(h, "POST", "/knowledge/rule/delete", hdr(ok), url.Values{"csrf": {csrf}, "id": {"1"}, "back": {"%zz"}}.Encode()); w.Header().Get("Location") != "/knowledge?deleted=1#kb" {
		t.Errorf("bare redirect %q", w.Header().Get("Location"))
	}
	if w := do(h, "POST", "/knowledge/rule/delete", hdr(ok), "%zz"); w.Code != http.StatusBadRequest {
		t.Errorf("bad form %d", w.Code)
	}
	// The cap itself is accepted.
	st.rules = nil
	for i := 1; i <= maxBulk; i++ {
		st.rules = append(st.rules, learn.Rule{ID: int64(i), Scope: learn.ScopeGlobal, Text: "r"})
	}
	if w := do(h, "POST", "/knowledge/rule/delete", hdr(ok), many(maxBulk, "")); w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "deleted=1%2C2%2C") {
		t.Errorf("cap: %d %q", w.Code, w.Header().Get("Location"))
	}

	// Store failures.
	st.failMut = true
	for _, path := range []string{"/knowledge/rule/delete", "/knowledge/rule/scope", "/knowledge/rule/restore"} {
		if w := do(h, "POST", path, hdr(ok), form("id", "1", "scope", "global")); w.Code != http.StatusInternalServerError {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}

func csrfFor(t *testing.T, h http.Handler) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(do(h, "GET", "/knowledge", nil, "").Body.String())
	if m == nil {
		t.Fatal("no csrf token")
	}
	return m[1]
}

// The filter row is a GET form enhanced by the filter bar (native selects
// keep the values; the period default is the server's 30 days).
func TestFilterBar(t *testing.T) {
	h, _ := testHandler(t, token)
	body := do(h, "GET", "/knowledge?period=90", map[string]string{"Authorization": "Bearer " + token}, "").Body.String()
	for _, want := range []string{
		`<form class="filters" method="get" action="/knowledge" role="search" aria-label="Фильтры" data-review-filters>`,
		`<select name="project" data-label="Проект" data-type="search-select">`, `<select name="period" data-label="Период" data-default="30">`,
		`<option value="90" selected>90 дней</option>`, `<button type="submit" class="btn sm" data-rf-fallback>Применить</button>`,
		`src="` + webassets.FiltersPath + `"`, `src="` + webassets.KnowledgePath + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if v := webui.InlineViolations(body); len(v) != 0 {
		t.Errorf("inline style/handler (blocked by the CSP): %q", v)
	}
	if strings.Contains(body, "TomSelect") || strings.Contains(body, "initCombos") || strings.Contains(body, "{{") {
		t.Error("leftovers of the old combobox or placeholders")
	}
}

var seriesRe = regexp.MustCompile(`(?s)<script type="application/json" id="k-series">(.*?)</script>`)

func TestBlocks(t *testing.T) {
	h, st := testHandler(t, "")
	body := do(h, "GET", "/knowledge", nil, "").Body.String()

	// Chart data: JSON in a non-executed script, escaped (no raw </script>),
	// the same window as the big number: the last accumulated point is the KPI.
	m := seriesRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no series")
	}
	if strings.Contains(m[1], "<") {
		t.Errorf("series not escaped: %s", m[1])
	}
	var pts []struct{ S, T, W, D, X string }
	if err := json.Unmarshal([]byte(m[1]), &pts); err != nil || len(pts) != 4 {
		t.Fatalf("series %v %s", err, m[1])
	}
	fixed := 0
	for _, p := range pts {
		if p.S == learn.OutcomeFixed {
			fixed++
		}
	}
	if got := strconv.Itoa(fixed*100/len(pts)) + "%"; got != "75%" || pts[1].T != "</script><b>x" || pts[2].T != "замечание без заголовка" ||
		pts[0].W != "g/api !12" || pts[0].X != "26.10" || pts[0].D != "26.10.2026 09:30" {
		t.Errorf("series %+v", pts)
	}
	if st.seriesArg.limit != maxSeries+1 || st.filter.Limit != maxDecisions+1 || st.filter.Offset != 0 || !st.filter.Since.Equal(now.AddDate(0, 0, -30)) {
		t.Errorf("reads: %+v %+v", st.seriesArg, st.filter)
	}

	for _, want := range []string{
		// where the findings went: one scale, widths relative to the largest count
		`<rect class="c-resolved_by_bot" width="100" height="6"/>`, `<rect class="c-fixed" width="75" height="6"/>`, "в точность не входит",
		// decisions: escaped title, excerpt code, MR link only for a safe path, rule links
		"&lt;/script&gt;&lt;img src=x onerror=alert(1)&gt;", "<code>errors.Is</code> &lt;b&gt;тут&lt;/b&gt;",
		`href="https://gitlab.example.com/g/api/-/merge_requests/12"`, `<span class="sev sev-critical">critical</span>`, "a.go:7",
		`href="/knowledge?kb=deleted&amp;rule=7#rule-7">#7 (удалено)</a>`, `href="/knowledge?rule=1#rule-1">#1</a>`,
		"После отклонения бот запомнил (правило удалено): «Старое правило»", "Текст замечания не сохранён.",
		`>принято <span class="n">3</span></a>`, `>опровергнуто проверкой <span class="n">0</span></a>`, "1–3 из 4",
		// per project: busiest first, empty rows marked, stacked bar with a tooltip
		`<a class="pname" href="/knowledge?project=g%2Fapi">g/api</a>`, `<tr data-empty>`, "data-tip=\"g/api\nПринято: 3\nОтклонено: 1\nЖдут решения: 2\"",
		`<rect class="c-fixed" x="0.0" width="50.0" height="10"/>`, `<rect class="c-dismissed" x="50.0" width="16.6" height="10"/>`,
		"скрыть проекты без решений и открытых замечаний (1)", `<span class="lown" title="меньше 10 решений">мало</span>`,
		// knowledge base: rules and memory in one table, selectable rules
		`name="id" value="1" form="kb-bulk"`, `value="3" form="kb-bulk" data-noproj`, `<tr class="mem">`, `formaction="/knowledge/rule/scope" name="scope" value="global"`,
		`>всё <span class="n">6</span></a>`, `>удалённые <span class="n">1</span></a>`, `data-max="200"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Index(body, `href="/knowledge?project=g%2Fapi"`) > strings.Index(body, `href="/knowledge?project=g%2Fweb"`) {
		t.Error("projects with decisions go first")
	}
	for _, bad := range []string{"<img src=x", `x" onclick`, "sev-x", "merge_requests/3\"", "Старое правило</td>", "<b>тут"} {
		if strings.Contains(body, bad) {
			t.Errorf("page has %q", bad)
		}
	}
	if v := webui.InlineViolations(body); len(v) != 0 {
		t.Errorf("inline style/handler: %q", v)
	}
}

func TestTabsPagingAndBanner(t *testing.T) {
	h, st := testHandler(t, "")
	get := func(q string) string {
		t.Helper()
		w := do(h, "GET", "/knowledge"+q, nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", q, w.Code)
		}
		if v := webui.InlineViolations(w.Body.String()); len(v) != 0 {
			t.Errorf("%s: inline style/handler: %q", q, v)
		}
		return w.Body.String()
	}
	// Deleted rules: restore, alone and in a group; highlighted when a decision points at it.
	b := get("?kb=deleted&rule=7")
	for _, want := range []string{`<tr class="rule gone hl" id="rule-7">`, `action="/knowledge/rule/restore"`, "Вернуть выбранные", "Удалено</th>",
		`<input type="hidden" name="kb" value="deleted">`} {
		if !strings.Contains(b, want) {
			t.Errorf("deleted tab lacks %q", want)
		}
	}
	if strings.Contains(b, "Не трогай vendor") || strings.Contains(b, "Удалить выбранные") {
		t.Error("deleted tab shows live rules or delete")
	}
	if b := get("?kb=memory"); strings.Contains(b, "Не трогай vendor") || !strings.Contains(b, "make build") || strings.Contains(b, `data-bulk`) {
		t.Error("memory tab")
	}
	if b := get("?kb=rules"); strings.Contains(b, "make build") || !strings.Contains(b, "Не трогай vendor") {
		t.Error("rules tab")
	}
	if b := get("?kb=bogus&outcome=bogus&dpage=0&rule=x"); !strings.Contains(b, `>всё <span class="n">6</span></a>`) || st.filter.Status != "" || st.filter.Offset != 0 {
		t.Error("bad parameters must fall back")
	}

	// Paging and the outcome filter.
	st.decisions = nil
	for i := 0; i < maxDecisions+5; i++ {
		st.decisions = append(st.decisions, learn.Outcome{ID: int64(i + 1), ProjectPath: "g/api", Status: learn.OutcomeFixed})
	}
	st.stats["g/api"] = learn.OutcomeStats{Fixed: maxDecisions + 5}
	b = get("?outcome=fixed&project=g/api")
	if st.filter.Status != learn.OutcomeFixed || st.filter.ProjectPath != "g/api" || st.seriesArg.project != "g/api" ||
		!strings.Contains(b, `href="/knowledge?dpage=2&amp;outcome=fixed&amp;project=g%2Fapi#decisions">Показать ещё`) || !strings.Contains(b, "1–200 из 205") {
		t.Errorf("page 1: %+v", st.filter)
	}
	b = get("?outcome=fixed&project=g/api&dpage=2")
	if st.filter.Offset != maxDecisions || !strings.Contains(b, "201–205 из 205") || strings.Contains(b, "Показать ещё") ||
		!strings.Contains(b, `href="/knowledge?outcome=fixed&amp;project=g%2Fapi#decisions">← предыдущие`) {
		t.Errorf("page 2: %+v", st.filter)
	}
	if b := get("?outcome=dismissed"); !strings.Contains(b, "Нет решений «отклонено»") {
		t.Error("empty outcome")
	}
	// A long series is cut, and said so.
	st.series = make([]learn.Outcome, maxSeries+1)
	for i := range st.series {
		st.series[i].Status = learn.OutcomeFixed
	}
	if b := get(""); !strings.Contains(b, "На графике первые 2000 решений") {
		t.Error("series truncation note")
	}

	// The banner: strictly validated result parameters.
	for q, want := range map[string]string{
		"?deleted=2":                     "Правило удалено",
		"?deleted=2,1":                   "Удалено 2 правила.",
		"?restored=5":                    "Возвращено 5 правил.",
		"?restored=21":                   "Возвращено 21 правило.",
		"?scoped=3&scope=global":         "Теперь действуют во всех проектах: 3 правила.",
		"?scoped=1&scope=project":        "Теперь действуют только в своём проекте: 1 правило.",
		"?conflict=7&live=1&conflicts=1": "Ничего не возвращено: правило #7 совпадает с действующим правилом #1.",
		"?conflict=7&live=1&conflicts=2": "Ничего не возвращено: 2 правила совпадают с действующими (например, #7 и #1).",
	} {
		if b := get(q); !strings.Contains(b, want) {
			t.Errorf("%s: banner lacks %q", q, want)
		}
	}
	b = get("?deleted=2,1")
	if !strings.Contains(b, `<input type="hidden" name="id" value="2"><input type="hidden" name="id" value="1">`) {
		t.Error("undo form")
	}
	tooMany := make([]string, maxBulk+1)
	for i := range tooMany {
		tooMany[i] = strconv.Itoa(i + 1)
	}
	for _, q := range []string{"?deleted=1,%3Cscript%3E", "?deleted=0", "?deleted=", "?deleted=1,,2", "?deleted=" + strings.Join(tooMany, ","),
		"?restored=0", "?restored=1000", "?restored=x", "?scoped=2&scope=all", "?scoped=2", "?conflict=7", "?conflict=x&live=1"} {
		if b := get(q); strings.Contains(b, `class="notice`) {
			t.Errorf("%s: banner shown", q)
		}
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{1: "правило", 2: "правила", 4: "правила", 5: "правил", 11: "правил", 12: "правил", 21: "правило", 22: "правила", 111: "правил", 0: "правил"} {
		if got := plural(n, "правило", "правила", "правил"); got != want {
			t.Errorf("%d: %q", n, got)
		}
	}
	if segments("") != nil || len(segments("a`b`c")) != 3 || !segments("a`b`")[1].Code {
		t.Error("segments")
	}
	h := &handler{o: Options{GitLabURL: "https://g.example"}}
	for p, ok := range map[string]bool{"g/a": true, "../a": false, "g/a b": false, "": false} {
		if (h.mrURL(p, 1) != "") != ok {
			t.Errorf("mrURL %q", p)
		}
	}
	if h.mrURL("g/a", 0) != "" || (&handler{}).mrURL("g/a", 1) != "" {
		t.Error("mrURL without iid or GitLab")
	}
}
