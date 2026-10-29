package usageweb

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Guard is the access check shared by the bot's pages (/usage, /runs): an
// optional token given as "Authorization: Bearer <token>" or once as
// "?token=<token>" (then kept as sha256(token) in an HttpOnly SameSite=Strict
// cookie scoped to CookiePath, and the token is removed from the URL by a
// redirect). Comparison is constant time. An empty token means open access.
type Guard struct {
	CookieName string
	CookiePath string
	// Hint is the page to name in the 401 message, e.g. "/usage".
	Hint string
	// EnvName is the env var to name in the 401 message.
	EnvName   string
	tokenHash []byte
}

// NewGuard builds a Guard; token "" = open.
func NewGuard(token, cookieName, cookiePath, hint, envName string) *Guard {
	g := &Guard{CookieName: cookieName, CookiePath: cookiePath, Hint: hint, EnvName: envName}
	if token != "" {
		s := sha256.Sum256([]byte(token))
		g.tokenHash = s[:]
	}
	return g
}

// Open reports whether no token is required.
func (g *Guard) Open() bool { return g.tokenHash == nil }

// Binding is hex(sha256(token)): the value of the auth cookie, the same for
// a Bearer request; "" when the guard is open. Pages derive their CSRF
// tokens from it (never from the token itself).
func (g *Guard) Binding() string {
	if g.tokenHash == nil {
		return ""
	}
	return hex.EncodeToString(g.tokenHash)
}

// Authorize checks the request. It may answer itself (401, or a redirect
// after a ?token= login) and then returns false.
func (g *Guard) Authorize(w http.ResponseWriter, r *http.Request) bool {
	if g.tokenHash == nil {
		return true
	}
	q := r.URL.Query()
	if q.Has("token") {
		if !g.match(q.Get("token")) {
			g.deny(w)
			return false
		}
		http.SetCookie(w, &http.Cookie{
			Name: g.CookieName, Value: hex.EncodeToString(g.tokenHash), Path: g.CookiePath,
			HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r),
			MaxAge: int((30 * 24 * time.Hour).Seconds()),
		})
		q.Del("token")
		u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
		return false
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		const p = "Bearer "
		if len(auth) > len(p) && strings.EqualFold(auth[:len(p)], p) && g.match(strings.TrimSpace(auth[len(p):])) {
			return true
		}
		g.deny(w)
		return false
	}
	if c, err := r.Cookie(g.CookieName); err == nil {
		if b, err := hex.DecodeString(c.Value); err == nil && subtle.ConstantTimeCompare(b, g.tokenHash) == 1 {
			return true
		}
	}
	g.deny(w)
	return false
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (g *Guard) match(token string) bool {
	s := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(s[:], g.tokenHash) == 1
}

func (g *Guard) deny(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="reviewbot"`)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, "Нужен токен: откройте %s?token=<%s> или передайте заголовок Authorization: Bearer <%s>.\n", g.Hint, g.EnvName, g.EnvName)
}

// SecurityHeaders sets the headers every page response gets.
func SecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
}

// SetChartPageCSP sets the Content-Security-Policy of the chart pages
// (/usage, /runs, /map): inline code and styles need the nonce; script-src
// 'self' lets the nonce-tagged <script src> of the embedded ECharts bundle
// (webassets.EChartsPath) load. ECharts styles its tooltip and canvas
// through CSSOM (element.style), which style-src does not restrict, and the
// pages' tooltip formatters emit markup without style attributes, so
// style-src stays nonce-only. Data endpoints are JSON/NDJSON with nosniff,
// so they cannot run as script.
func SetChartPageCSP(w http.ResponseWriter, nonce string) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"' 'self'; style-src 'nonce-"+nonce+
		"'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

// Nonce returns a fresh CSP nonce.
func Nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}
