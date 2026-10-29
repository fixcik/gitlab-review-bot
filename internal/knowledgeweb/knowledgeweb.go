// Package knowledgeweb serves the page of what the bot has learned:
//
//	GET  /knowledge               precision over the team's decisions, where the findings went, the
//	                              decisions table, the knowledge base (rules and project memory, deleted
//	                              rules), per-project outcomes, link to the system map
//	POST /knowledge/rule/delete   delete rules (form: id ×1..200, csrf, back)
//	POST /knowledge/rule/scope    make rules global or project-only (form: id ×1..200, scope, csrf, back)
//	POST /knowledge/rule/restore  restore deleted rules (form: id ×1..200, csrf, back)
//
// The page is rendered on the server with html/template (every string is
// escaped; rules, memory, findings and note links come from users and from
// the model and are untrusted); the page script (webassets.KnowledgePath)
// draws the chart and drives the selection, without network access. Access
// works like /usage (usageweb.Guard): optional token, enabled flag, startup
// warning when open. The POSTs additionally need a same-origin Origin (or
// Referer) and a CSRF token derived from the auth binding with a per-process
// secret, like the "hide" button of /problems. Every POST is all or nothing:
// a malformed id, an id that is not a rule of the right kind, too many ids
// or a restore that would duplicate a live rule change nothing.
package knowledgeweb

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/usageweb"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// Paths.
const (
	PagePath    = "/knowledge"
	DeletePath  = "/knowledge/rule/delete"
	ScopePath   = "/knowledge/rule/scope"
	RestorePath = "/knowledge/rule/restore"
)

const cookieName = "reviewbot_knowledge"

const (
	// maxRules bounds the rules table (live and deleted each).
	maxRules = 1000
	// maxBulk: rule ids per POST.
	maxBulk = 200
	// maxBodySize: 200 × "id=<18 digits>&" ≈ 4 KB plus csrf, scope and back.
	maxBodySize = 16 << 10
	// maxDecisions: rows of the decisions table per page («показать ещё»).
	maxDecisions = 200
	// maxSeries: points of the precision chart.
	maxSeries = 2000
	// lowDecisions: below this many decisions the precision is marked
	// «мало решений».
	lowDecisions = 10
)

// Store is what the page reads and writes (*store.SQLite).
type Store interface {
	ListAllLearnings(ctx context.Context, limit int) ([]learn.Rule, error)
	ListDeletedLearnings(ctx context.Context, limit int) ([]learn.Rule, error)
	DeleteLearnings(ctx context.Context, ids []int64) error
	SetLearningsScope(ctx context.Context, ids []int64, scope string) error
	UndeleteLearnings(ctx context.Context, ids []int64) (conflicts map[int64]int64, err error)
	ListAllMemory(ctx context.Context) ([]memory.Section, error)
	OutcomeStatsAll(ctx context.Context, since time.Time) (map[string]learn.OutcomeStats, error)
	ListDecisions(ctx context.Context, f learn.DecisionFilter) ([]learn.Outcome, error)
	DecisionSeries(ctx context.Context, project string, since time.Time, limit int) ([]learn.Outcome, error)
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
	// GitLabURL restricts the source links of rules to this GitLab and is
	// the base of the MR links of findings.
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

// New returns the handler (mount it on PagePath and the POST paths).
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
		o.Logger.Info("knowledge page disabled")
	case o.Token == "":
		o.Logger.Warn("knowledge page " + PagePath + " is open WITHOUT authentication: anyone who can reach the bot's port " +
			"sees the team's rules, the projects' memory and finding statistics, and can delete rules. To protect it set " +
			"USAGE_TOKEN (or usage.token) and open " + PagePath + "?token=<token> once")
	default:
		o.Logger.Info("knowledge page enabled with token", "path", PagePath)
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.o.Enabled || h.o.Store == nil {
		http.NotFound(w, r)
		return
	}
	var do action
	switch r.URL.Path {
	case PagePath:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	case DeletePath, ScopePath, RestorePath:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		do = map[string]action{DeletePath: h.deleteRules, ScopePath: h.setScope, RestorePath: h.restoreRules}[r.URL.Path]
	default:
		http.NotFound(w, r)
		return
	}
	usageweb.SecurityHeaders(w)
	w.Header().Set("Referrer-Policy", "same-origin")
	if !h.guard.Authorize(w, r) {
		return
	}
	if do != nil {
		h.act(w, r, do)
		return
	}
	h.page(w, r)
}

// csrfToken is HMAC(secret, "knowledge" + auth binding): only a page served
// to someone who passed the guard has it, and it changes with the access
// token and with every restart.
func (h *handler) csrfToken() string {
	m := hmac.New(sha256.New, h.secret)
	m.Write([]byte("knowledge\x00" + h.guard.Binding()))
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

// action changes the rules ids (validated, deduplicated, 1..maxBulk) and
// reports either a refusal (code != 0: HTTP status and message, nothing was
// changed) or the result parameters of the redirect (for the banner).
type action func(ctx context.Context, f url.Values, ids []int64) (code int, msg string, result url.Values)

// idRe: a positive decimal id that fits int64.
var idRe = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// parseIDs reads every "id" of the form: 1..maxBulk strictly valid ids
// (duplicates dropped, order kept); ok is false on anything else.
func parseIDs(f url.Values) (ids []int64, msg string) {
	raw := f["id"]
	switch {
	case len(raw) == 0:
		return nil, "no rule id: select a rule"
	case len(raw) > maxBulk:
		return nil, "too many rules: at most " + strconv.Itoa(maxBulk) + " at once"
	}
	seen := map[int64]bool{}
	for _, s := range raw {
		if !idRe.MatchString(s) {
			return nil, "bad rule id"
		}
		id, _ := strconv.ParseInt(s, 10, 64) // idRe: always fits
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, ""
}

// act runs a POST action after the origin, form, CSRF and id checks, then
// redirects back to the page with the same filters and the result.
func (h *handler) act(w http.ResponseWriter, r *http.Request, do action) {
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
	ids, msg := parseIDs(r.PostForm)
	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	code, msg, result := do(r.Context(), r.PostForm, ids)
	if code != 0 {
		http.Error(w, msg, code)
		return
	}
	back := backValues(r.PostForm.Get("back"))
	for k, v := range result {
		back[k] = v
	}
	target := PagePath
	if len(back) > 0 {
		target += "?" + back.Encode()
	}
	http.Redirect(w, r, target+"#kb", http.StatusSeeOther)
}

// backKeys are the page parameters a POST keeps when it redirects back.
var backKeys = []string{"project", "period", "kb", "outcome", "dpage"}

// backValues keeps only the page parameters of a "back" query (their values
// are validated again by the page).
func backValues(raw string) url.Values {
	back := url.Values{}
	if q, err := url.ParseQuery(raw); err == nil {
		for _, k := range backKeys {
			if v := q.Get(k); v != "" && len(v) <= 512 {
				back.Set(k, v)
			}
		}
	}
	return back
}

func joinIDs(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(s, ",")
}

// storeRefusal maps an error of a bulk rule change to the HTTP refusal.
func (h *handler) storeRefusal(what string, ids []int64, err error) (int, string, url.Values) {
	switch {
	case errors.Is(err, learn.ErrRuleNotFound):
		return http.StatusNotFound, "rule not found (deleted or restored meanwhile?): reload the page", nil
	case errors.Is(err, learn.ErrRuleNoProject):
		return http.StatusBadRequest, "a selected rule has no project to be limited to", nil
	}
	h.o.Logger.Error("knowledge: "+what, "ids", ids, "err", err)
	return http.StatusInternalServerError, "internal error", nil
}

func (h *handler) deleteRules(ctx context.Context, _ url.Values, ids []int64) (int, string, url.Values) {
	if err := h.o.Store.DeleteLearnings(ctx, ids); err != nil {
		return h.storeRefusal("delete rules", ids, err)
	}
	h.o.Logger.Info("knowledge: rules deleted", "ids", ids)
	return 0, "", url.Values{"deleted": {joinIDs(ids)}}
}

func (h *handler) setScope(ctx context.Context, f url.Values, ids []int64) (int, string, url.Values) {
	scope := f.Get("scope")
	if scope != learn.ScopeGlobal && scope != learn.ScopeProject {
		return http.StatusBadRequest, "bad scope", nil
	}
	if err := h.o.Store.SetLearningsScope(ctx, ids, scope); err != nil {
		return h.storeRefusal("set scope", ids, err)
	}
	h.o.Logger.Info("knowledge: rule scope changed", "ids", ids, "scope", scope)
	return 0, "", url.Values{"scoped": {strconv.Itoa(len(ids))}, "scope": {scope}}
}

func (h *handler) restoreRules(ctx context.Context, _ url.Values, ids []int64) (int, string, url.Values) {
	conflicts, err := h.o.Store.UndeleteLearnings(ctx, ids)
	if err != nil {
		return h.storeRefusal("restore rules", ids, err)
	}
	for _, id := range ids { // the first conflict in the order of the form
		if live, ok := conflicts[id]; ok {
			h.o.Logger.Info("knowledge: restore refused, an equal rule is live", "ids", ids, "conflicts", conflicts)
			return 0, "", url.Values{"conflict": {strconv.FormatInt(id, 10)}, "live": {strconv.FormatInt(live, 10)},
				"conflicts": {strconv.Itoa(len(conflicts))}}
		}
	}
	h.o.Logger.Info("knowledge: rules restored", "ids", ids)
	return 0, "", url.Values{"restored": {strconv.Itoa(len(ids))}}
}
