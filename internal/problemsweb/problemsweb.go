// Package problemsweb serves the verification problems page:
//
//	GET  /problems         what the bot could not check (filters: kind, project, status, impact, q; sort)
//	POST /problems/ack     hide problems until they are seen again (form: fingerprint (one or more), csrf, back)
//	POST /problems/unack   return hidden problems to the active ones ("Вернуть"; same form)
//
// The page is rendered on the server with html/template (every string is
// escaped; subjects and details come from tools and from the review model
// and are untrusted). Access works like /usage (usageweb.Guard): optional
// token, enabled flag, startup warning when open. The POST additionally
// needs a same-origin Origin (or Referer) and a CSRF token derived from the
// auth binding (sha256 of the token) with a per-process secret; both POSTs
// share the checks (mutate). The page works without JS (plain GET and POST
// forms); problems.js (webassets.ProblemsPath) adds the text search without
// a reload, row selection, "Как исправить" toggles and snippet copying.
package problemsweb

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/usageweb"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// Paths.
const (
	PagePath  = "/problems"
	AckPath   = "/problems/ack"
	UnackPath = "/problems/unack"
)

const cookieName = "reviewbot_problems"

// maxRows bounds the table.
const maxRows = 1000

// Limits of the POSTs: fingerprints per request (a whole table) and the
// body (1000 × "fingerprint=<32 hex>&" ≈ 45 KB plus csrf and back).
const (
	maxBulk     = maxRows
	maxBodySize = 64 << 10
	// maxUndo: up to this many hidden fingerprints go into the redirect
	// (?hidden=fp,fp…) for the "Вернуть" button; more give only a count.
	maxUndo = 100
	// maxQueryRunes caps the text search.
	maxQueryRunes = 200
)

// Store is what the page reads and writes (*store.SQLite).
type Store interface {
	ListProblems(ctx context.Context, f problems.Filter) ([]problems.Record, error)
	AckProblem(ctx context.Context, fingerprint string, at time.Time) (bool, error)
	UnackProblem(ctx context.Context, fingerprint string) (bool, error)
}

// Options configure the handler.
type Options struct {
	// Nav is the shared header (switched-off pages, problems badge).
	Nav   webui.Nav
	Store Store
	// Enabled=false makes every endpoint answer 404.
	Enabled bool
	// Token, when non-empty, is required (see usageweb.Guard).
	Token string
	// GitLabURL builds MR links.
	GitLabURL string
	// Location formats times (default time.Local).
	Location *time.Location
	Now      func() time.Time
	Logger   *slog.Logger
}

type handler struct {
	o      Options
	guard  *usageweb.Guard
	secret []byte // per process: CSRF tokens are valid until a restart
}

// New returns the handler (mount it on PagePath and AckPath).
func New(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	o.GitLabURL = strings.TrimRight(o.GitLabURL, "/")
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return &handler{o: o, secret: secret,
		guard: usageweb.NewGuard(o.Token, cookieName, PagePath, PagePath, "USAGE_TOKEN")}
}

// Register mounts the handler and logs how the page is protected.
func Register(mux *http.ServeMux, o Options) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	h := New(o)
	mux.Handle(PagePath, h)
	mux.Handle(PagePath+"/", h)
	switch {
	case !o.Enabled || o.Store == nil:
		o.Logger.Info("problems page disabled (review.problems.enabled: false)")
	case o.Token == "":
		o.Logger.Warn("problems page " + PagePath + " is open WITHOUT authentication: anyone who can reach the bot's port " +
			"sees repositories, error texts and what the bot could not check, and can hide problems. To protect it set " +
			"USAGE_TOKEN (or usage.token) and open " + PagePath + "?token=<token> once")
	default:
		o.Logger.Info("problems page enabled with token", "path", PagePath)
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.o.Enabled || h.o.Store == nil {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case PagePath:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	case AckPath, UnackPath:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	usageweb.SecurityHeaders(w)
	// Same-origin requests carry Origin (and Referer), so the ack form passes
	// the origin check; cross-origin navigation still gets no referrer.
	w.Header().Set("Referrer-Policy", "same-origin")
	if !h.guard.Authorize(w, r) {
		return
	}
	if r.URL.Path != PagePath {
		h.mutate(w, r, r.URL.Path == UnackPath)
		return
	}
	h.page(w, r)
}

// csrfToken is HMAC(secret, "problems-ack" + auth binding): only a page
// served to someone who passed the guard has it, and it changes with the
// access token and with every restart.
func (h *handler) csrfToken() string {
	m := hmac.New(sha256.New, h.secret)
	m.Write([]byte("problems-ack\x00" + h.guard.Binding()))
	return hex.EncodeToString(m.Sum(nil))
}

// sameOrigin: Origin (or, without it, Referer) must name this host. A
// missing or "null" origin is refused.
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" || src == "null" {
		src = r.Header.Get("Referer")
	}
	if src == "" || src == "null" {
		return false
	}
	u, err := url.Parse(src)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

var fingerprintRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// mutate is POST /problems/ack (unack=false) and /problems/unack: the same
// checks for both (same origin, body limit, CSRF token, every fingerprint
// strictly validated, at most maxBulk), then one store call per
// fingerprint and a redirect back to the page with the filters of the form
// and the result (?hidden=… / ?restored=N) for the banner.
func (h *handler) mutate(w http.ResponseWriter, r *http.Request, unack bool) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !hmac.Equal([]byte(r.PostForm.Get("csrf")), []byte(h.csrfToken())) {
		http.Error(w, "bad csrf token: reload the page", http.StatusForbidden)
		return
	}
	raw := r.PostForm["fingerprint"]
	switch {
	case len(raw) == 0:
		http.Error(w, "no fingerprint: select a problem", http.StatusBadRequest)
		return
	case len(raw) > maxBulk:
		http.Error(w, "too many fingerprints", http.StatusBadRequest)
		return
	}
	var fps []string
	seen := map[string]bool{}
	for _, fp := range raw {
		if !fingerprintRe.MatchString(fp) {
			http.Error(w, "bad fingerprint", http.StatusBadRequest)
			return
		}
		if !seen[fp] {
			seen[fp] = true
			fps = append(fps, fp)
		}
	}
	var changed []string
	for _, fp := range fps {
		var ok bool
		var err error
		if unack {
			ok, err = h.o.Store.UnackProblem(r.Context(), fp)
		} else {
			ok, err = h.o.Store.AckProblem(r.Context(), fp, h.o.Now())
		}
		if err != nil {
			h.o.Logger.Error("problems: change", "unack", unack, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if ok {
			changed = append(changed, fp)
		}
	}
	back := backValues(r.PostForm.Get("back"))
	if unack {
		h.o.Logger.Info("problems: returned to active", "count", len(changed), "fingerprints", changed)
		if len(changed) > 0 {
			back.Set("restored", strconv.Itoa(len(changed)))
		}
	} else {
		h.o.Logger.Info("problems: acknowledged", "count", len(changed), "fingerprints", changed)
		switch {
		case len(changed) > maxUndo:
			back.Set("hidden", strconv.Itoa(len(changed)))
		case len(changed) > 0:
			back.Set("hidden", strings.Join(changed, ","))
		}
	}
	target := PagePath
	if len(back) > 0 {
		target += "?" + back.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// backKeys are the page parameters a POST keeps when it redirects back.
var backKeys = []string{"kind", "project", "status", "impact", "q", "sort"}

// backValues keeps only the page parameters of a "back" query (their values
// are validated again by the page).
func backValues(raw string) url.Values {
	back := url.Values{}
	if q, err := url.ParseQuery(raw); err == nil {
		for _, k := range backKeys {
			if v := q.Get(k); v != "" && len(v) <= 4*maxQueryRunes {
				back.Set(k, v)
			}
		}
	}
	return back
}
