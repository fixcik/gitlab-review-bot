package setupweb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/doctor"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

func get(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func sample() doctor.Report {
	return doctor.Report{Fails: 1, Warns: 1, Results: []doctor.Result{
		{ID: "config.valid", Group: "Конфигурация", Title: "/etc/reviewbot/config.yaml", Status: doctor.OK, Detail: "проходит проверку serve"},
		{ID: "gitlab.version", Group: "GitLab", Title: "адрес", Status: doctor.OK, Detail: "https://gitlab.example.com, GitLab 17.5.1"},
		{ID: "gitlab.service_token", Group: "GitLab", Title: "служебный токен", Status: doctor.Warn, Detail: "не задан", Action: "GITLAB_SERVICE_TOKEN в .env"},
		{ID: "docker.sandbox_image", Group: "Docker", Title: "образ песочницы", Status: doctor.Fail, Detail: "не скачан <img>", Action: "docker compose pull"},
		{ID: "egress.proxy", Group: "Egress", Title: "прокси", Status: doctor.Skip, Detail: "не задан"},
		{ID: "claude.token", Group: "Claude", Title: "токен", Status: doctor.OK, Detail: "принят", Action: "never shown for ok"},
	}}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestPage(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	runs := 0
	var deadline bool
	h := New(Options{Now: c.now, Run: func(ctx context.Context) doctor.Report {
		runs++
		_, deadline = ctx.Deadline()
		return sample()
	}})
	w := get(h, "GET", "/setup", nil)
	if w.Code != http.StatusOK || runs != 1 || !deadline {
		t.Fatalf("code %d runs %d deadline %v", w.Code, runs, deadline)
	}
	body := w.Body.String()
	for _, want := range []string{"<h1>Проверка настройки</h1>", `<b>3</b> <span>пройдено</span>`, `n fail"><b>1</b> <span>ошибка</span>`, `n warn"><b>1</b> <span>предупреждение</span>`,
		`<h2>GitLab</h2>`, `<h2>Docker</h2>`, `id="c-docker.sandbox_image"`, `<li class="chk fail"`, "не скачан &lt;img&gt;", "docker compose pull",
		`href="?refresh=1"`, `href="/setup/data.json"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "never shown for ok") || strings.Count(body, "<h2>GitLab</h2>") != 1 {
		t.Error("an OK check shows its action, or a group is split")
	}
	if p := webui.HeaderProblems(body, webui.SectionSetup); len(p) > 0 {
		t.Errorf("header: %v", p)
	}
	if v := webui.InlineViolations(body); len(v) > 0 {
		t.Errorf("inline style/handlers: %v", v)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'none'") || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", w.Header())
	}

	// Cached for TTL; a refresh within MinRefresh is the cached report too.
	c.t = c.t.Add(10 * time.Second)
	get(h, "GET", "/setup?refresh=1", nil)
	get(h, "GET", "/setup/data.json", nil)
	if runs != 1 {
		t.Fatalf("runs %d, want 1 (cache)", runs)
	}
	c.t = c.t.Add(10 * time.Second) // 20 s: a refresh runs again, a plain view does not
	get(h, "GET", "/setup", nil)
	if runs != 1 {
		t.Fatalf("runs %d, want 1 (within TTL)", runs)
	}
	get(h, "GET", "/setup?refresh=1", nil)
	if runs != 2 {
		t.Fatalf("runs %d, want 2 (refresh)", runs)
	}
	c.t = c.t.Add(2 * time.Minute)
	w = get(h, "GET", "/setup/data.json", nil)
	var rep doctor.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil || runs != 3 || len(rep.Results) != 6 || !rep.At.Equal(c.t) || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("data: %v runs %d %+v", err, runs, rep)
	}
	// HEAD, wrong method.
	if w := get(h, "HEAD", "/setup", nil); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD page: %d %d", w.Code, w.Body.Len())
	}
	if w := get(h, "HEAD", "/setup/data.json", nil); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD data: %d", w.Code)
	}
	if w := get(h, "POST", "/setup", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestAuthAndRegister(t *testing.T) {
	o := Options{Token: "page-token-123456", Run: func(context.Context) doctor.Report { return doctor.Report{} }}
	mux := http.NewServeMux()
	Register(mux, o)
	for _, p := range []string{"/setup", "/setup/data.json"} {
		if w := get(mux, "GET", p, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token: %d", p, w.Code)
		}
	}
	if w := get(mux, "GET", "/setup", map[string]string{"Authorization": "Bearer page-token-123456"}); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `<b>0</b> <span>ошибок</span>`) {
		t.Fatalf("bearer: %d", w.Code)
	}
	if w := get(mux, "GET", "/setup/other", nil); w.Code != http.StatusNotFound {
		t.Fatalf("other path: %d", w.Code)
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{0: "ошибок", 1: "ошибка", 2: "ошибки", 5: "ошибок", 11: "ошибок", 21: "ошибка", 22: "ошибки", 112: "ошибок"} {
		if got := plural(n, "ошибка", "ошибки", "ошибок"); got != want {
			t.Errorf("plural(%d) = %q", n, got)
		}
	}
}
