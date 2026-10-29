// Package homeweb serves the main page "/": the bot's state for the last 24
// hours and links to its pages. A status line (the last task, tasks of the
// day, the queue), four headline numbers (Claude runs and spend with hourly
// sparklines, active problems by impact, learned rules with the precision of
// the findings), the block «Требует внимания» (active problems, failed and
// postponed tasks of the day, as tabs that work without JS) and a card per
// page with one figure of its own.
//
// Access works like /usage (usageweb.Guard): the same optional token. The
// page shows aggregates and, of the tasks, only what /usage shows of runs
// (kind, project, MR number and title, time, status); never a task's error
// or reason text, prompts or answers (those stay behind the /runs token).
// Pages that are switched off get no link, no card and no figure.
//
// Everything is rendered on the server (the CSP allows no inline style);
// home.js (webassets.HomePath) only writes the hovered hour of a sparkline
// next to its label.
package homeweb

import (
	"context"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/diag/diagindex"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/usageweb"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// PagePath is the only path served.
const PagePath = "/"

const cookieName = "reviewbot_home"

// Store is what the page reads (*store.SQLite), read-only.
type Store interface {
	// Claude runs (/usage).
	RunTotals(ctx context.Context, f meter.RunFilter) (meter.RunStats, error)
	RunsHourly(ctx context.Context, since, until time.Time) ([]meter.RunStats, error)
	RunsBy(ctx context.Context, f meter.RunFilter, dim meter.Dimension) ([]meter.RunGroup, error)
	// Problems (/problems).
	ListProblems(ctx context.Context, f problems.Filter) ([]problems.Record, error)
	// Knowledge (/knowledge).
	ListAllLearnings(ctx context.Context, limit int) ([]learn.Rule, error)
	ListAllMemory(ctx context.Context) ([]memory.Section, error)
	OutcomeStatsAll(ctx context.Context, since time.Time) (map[string]learn.OutcomeStats, error)
	// The diagnostics index (/runs).
	DiagRunSummary(ctx context.Context, since time.Time) (diagindex.Summary, error)
	ListDiagRuns(ctx context.Context, f diagindex.Filter) ([]diagindex.Row, error)
	// The persisted queue.
	ListJobs(ctx context.Context) ([]queue.Stored, error)
	ListPending(ctx context.Context) ([]queue.Pending, error)
}

// MapSource is the system map (*sysmap.Map).
type MapSource interface {
	Index() *sysmap.Index
}

// Options configure the handler.
type Options struct {
	// Nav is the shared header (switched-off pages, problems badge).
	Nav   webui.Nav
	Store Store
	// Token, when non-empty, is required (see usageweb.Guard).
	Token string
	// Which pages exist: a switched-off page gets no link and no card, and
	// its figures are not read.
	Usage, Diag, Problems bool
	// Map is the system map; nil = /map is off.
	Map MapSource
	// Location formats the hours (default time.Local, like /usage).
	Location *time.Location
	Now      func() time.Time
	Logger   *slog.Logger
}

type handler struct {
	o     Options
	guard *usageweb.Guard
}

// New returns the handler (mount it on "/": any other path answers 404).
func New(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	return &handler{o: o, guard: usageweb.NewGuard(o.Token, cookieName, PagePath, PagePath, "USAGE_TOKEN")}
}

// Register mounts the handler on "/" (the catch-all of the mux: every more
// specific route wins).
func Register(mux *http.ServeMux, o Options) { mux.Handle(PagePath, New(o)) }

//go:embed page.html
var pageHTML string

//go:embed home.css
var pageCSS string

var pageTmpl = template.Must(template.New("home").Parse(pageHTML))

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != PagePath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	usageweb.SecurityHeaders(w)
	if !h.guard.Authorize(w, r) {
		return
	}
	nonce := usageweb.Nonce()
	css, js := webui.Assets()
	d := h.build(r.Context())
	d.Nonce, d.CommonCSS, d.CommonJS, d.PageJS = nonce, template.CSS(css+"\n"+pageCSS), template.JS(js), webassets.HomePath

	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+
		"'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	if err := pageTmpl.Execute(w, d); err != nil {
		h.o.Logger.Error("home: render", "err", err)
	}
}
