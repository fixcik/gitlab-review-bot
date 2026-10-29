package usageweb

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const guardSecret = "s3cret-token"

// guarded serves "PRIVATE-DATA" only when g lets the request through, the
// way every page uses the Guard.
func guarded(g *Guard, target string, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	if g.Authorize(rec, req) {
		_, _ = rec.WriteString("PRIVATE-DATA")
	}
	return rec
}

func hashCookie(token string) *http.Cookie {
	s := sha256.Sum256([]byte(token))
	return &http.Cookie{Name: "c", Value: hex.EncodeToString(s[:])}
}

func TestGuardOpenWhenTokenUnset(t *testing.T) {
	g := NewGuard("", "c", "/p", "/p", "X_TOKEN")
	if !g.Open() {
		t.Fatal("empty token must mean open access")
	}
	rec := guarded(g, "/p?token=whatever", map[string]string{"Authorization": "Bearer junk"})
	if rec.Code != http.StatusOK || rec.Body.String() != "PRIVATE-DATA" {
		t.Fatalf("open guard: %d %q", rec.Code, rec.Body)
	}
	if NewGuard(guardSecret, "c", "/p", "/p", "X_TOKEN").Open() {
		t.Fatal("a token must close the guard")
	}
}

// Every refusal is a 401 with the hint only: never the protected content.
func TestGuardDeniesWithoutLeaking(t *testing.T) {
	g := NewGuard(guardSecret, "c", "/p", "/p", "X_TOKEN")
	for _, tc := range []struct {
		name    string
		target  string
		hdr     map[string]string
		cookies []*http.Cookie
	}{
		{name: "nothing", target: "/p"},
		{name: "wrong query", target: "/p?token=nope"},
		{name: "empty query", target: "/p?token="},
		{name: "wrong query with valid cookie", target: "/p?token=nope", cookies: []*http.Cookie{hashCookie(guardSecret)}},
		{name: "wrong bearer", target: "/p", hdr: map[string]string{"Authorization": "Bearer nope"}},
		{name: "empty bearer", target: "/p", hdr: map[string]string{"Authorization": "Bearer "}},
		{name: "bare scheme", target: "/p", hdr: map[string]string{"Authorization": "Bearer"}},
		{name: "token without scheme", target: "/p", hdr: map[string]string{"Authorization": guardSecret}},
		{name: "wrong bearer with valid cookie", target: "/p", hdr: map[string]string{"Authorization": "Bearer nope"}, cookies: []*http.Cookie{hashCookie(guardSecret)}},
		{name: "raw token as cookie", target: "/p", cookies: []*http.Cookie{{Name: "c", Value: guardSecret}}},
		{name: "cookie of another token", target: "/p", cookies: []*http.Cookie{hashCookie("other")}},
		{name: "truncated hash cookie", target: "/p", cookies: []*http.Cookie{{Name: "c", Value: hashCookie(guardSecret).Value[:32]}}},
		{name: "valid hash under another cookie name", target: "/p", cookies: []*http.Cookie{{Name: "other", Value: hashCookie(guardSecret).Value}}},
	} {
		rec := guarded(g, tc.target, tc.hdr, tc.cookies...)
		if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "PRIVATE-DATA") {
			t.Errorf("%s: %d %q", tc.name, rec.Code, rec.Body)
		}
		if rec.Header().Get("WWW-Authenticate") == "" || len(rec.Result().Cookies()) != 0 {
			t.Errorf("%s: headers %v", tc.name, rec.Header())
		}
		if strings.Contains(rec.Body.String(), guardSecret) {
			t.Errorf("%s: token echoed", tc.name)
		}
	}
}

func TestGuardAccepts(t *testing.T) {
	g := NewGuard(guardSecret, "c", "/p", "/p", "X_TOKEN")
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"bearer":            guarded(g, "/p", map[string]string{"Authorization": "Bearer " + guardSecret}),
		"lowercase scheme":  guarded(g, "/p", map[string]string{"Authorization": "bearer " + guardSecret}),
		"padded token":      guarded(g, "/p", map[string]string{"Authorization": "Bearer  " + guardSecret + " "}),
		"cookie":            guarded(g, "/p", nil, hashCookie(guardSecret)),
		"cookie, sub-route": guarded(g, "/p/data.json?x=1", nil, hashCookie(guardSecret)),
	} {
		if rec.Code != http.StatusOK || rec.Body.String() != "PRIVATE-DATA" {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Body)
		}
	}
}

// ?token= logs in: a redirect without the token (other parameters kept),
// the cookie holds only the hash, and the response itself carries no data.
func TestGuardQueryLogin(t *testing.T) {
	g := NewGuard(guardSecret, "c", "/p", "/p", "X_TOKEN")
	rec := guarded(g, "/p/sub?a=1&token="+guardSecret+"&b=2", nil)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Body.String(), "PRIVATE-DATA") {
		t.Fatalf("login: %d %q", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "/p/sub?a=1&b=2" {
		t.Errorf("location %q", loc)
	}
	cs := rec.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("cookies %+v", cs)
	}
	c := cs[0]
	if c.Name != "c" || c.Value != hashCookie(guardSecret).Value || c.Path != "/p" || !c.HttpOnly ||
		c.SameSite != http.SameSiteStrictMode || c.Secure || c.MaxAge <= 0 {
		t.Errorf("cookie %+v", c)
	}
	// Behind a TLS-terminating proxy the cookie is Secure.
	rec = guarded(g, "/p?token="+guardSecret, map[string]string{"X-Forwarded-Proto": "https"})
	if cs := rec.Result().Cookies(); len(cs) != 1 || !cs[0].Secure {
		t.Errorf("https cookie %+v", cs)
	}
}

// The usage page answers refusals and wrong methods with the hardening
// headers and no data.
func TestUsageRefusalHeaders(t *testing.T) {
	h := newHandler(t, Options{Enabled: true, Token: "s3cret"})
	rec := get(h, DataPath, nil)
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "group/api") || strings.Contains(rec.Body.String(), "alice") {
		t.Fatalf("401: %d %q", rec.Code, rec.Body)
	}
	for k, v := range map[string]string{"Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"} {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	req := httptest.NewRequest(http.MethodPost, DataPath, nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: %d allow=%q", w.Code, w.Header().Get("Allow"))
	}
}
