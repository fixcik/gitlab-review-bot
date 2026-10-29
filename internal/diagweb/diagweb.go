// Package diagweb serves the diagnostics pages of recent runs:
//
//	GET /runs                        list (HTML; data from /runs/data.json)
//	GET /runs/data.json              index rows of the window, filters applied
//	GET /runs/<id>                   one run (HTML; data from /runs/<id>/data.json)
//	GET /runs/<id>/data.json         run.json of the run
//	GET /runs/<id>/claude-<n>.jsonl  raw stream-json transcript (download)
//
// The pages are self-contained HTML (inline CSS/JS from internal/webui and
// diag.css under a nonce CSP, plus the filter bar and the run-detail
// renderer from internal/webassets, shared by /runs/<id> and the side panel
// of /runs); every string from the data is inserted as text, never as HTML. Access works like /usage
// (usageweb.Guard): optional token, enabled flag, startup warning when open.
// Unlike /usage these pages show prompts, code and Claude's answers.
package diagweb

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/usageweb"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// Paths.
const (
	ListPath = "/runs"
	DataPath = "/runs/data.json"
	prefix   = "/runs/"
)

const cookieName = "reviewbot_diag"

// maxRows bounds the list.
const maxRows = 3000

// Options configure the handler.
type Options struct {
	// Nav is the shared header (switched-off pages, problems badge).
	Nav    webui.Nav
	Reader *diag.Reader
	// Enabled=false makes every endpoint answer 404.
	Enabled bool
	// Token, when non-empty, is required (see usageweb.Guard).
	Token string
	// Retention is the default list window (diag.retention).
	Retention time.Duration
	// TraceURL is tracing.ui_url: the run page links the run's trace with
	// "{trace_id}" replaced ("" = the id is shown without a link).
	TraceURL string
	// Queue lists the persisted queue (store.SQLite, read-only) for the
	// "Сейчас" strip of /runs (ListData.Queue); nil = no queue block.
	Queue  QueueSource
	Now    func() time.Time
	Logger *slog.Logger
}

// QueueSource is the read-only view of the persisted queue: the job table
// (queue_jobs) and the pending automatic reviews (queue_pending).
type QueueSource interface {
	ListJobs(ctx context.Context) ([]queue.Stored, error)
	ListPending(ctx context.Context) ([]queue.Pending, error)
}

type handler struct {
	o     Options
	guard *usageweb.Guard
}

// New returns the handler (mount it on ListPath and ListPath+"/").
func New(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Retention <= 0 {
		o.Retention = diag.DefaultRetention
	}
	return &handler{o: o, guard: usageweb.NewGuard(o.Token, cookieName, ListPath, ListPath, "DIAG_TOKEN")}
}

// Register mounts the handler and logs how the pages are protected.
func Register(mux *http.ServeMux, o Options) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	h := New(o)
	mux.Handle(ListPath, h)
	mux.Handle(prefix, h)
	switch {
	case !o.Enabled:
		o.Logger.Info("diagnostics pages disabled (diag.enabled: false)")
	case o.Token == "":
		o.Logger.Warn("diagnostics pages " + ListPath + " are open WITHOUT authentication: anyone who can reach the bot's port " +
			"sees prompts, code, Claude transcripts and answers of the recent runs. To protect them set DIAG_TOKEN " +
			"(or diag.token, or USAGE_TOKEN) and open " + ListPath + "?token=<token> once; to turn them off set diag.enabled: false")
	default:
		o.Logger.Info("diagnostics pages enabled with token", "path", ListPath)
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.o.Enabled || h.o.Reader == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	route, id, n, ok := parsePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	usageweb.SecurityHeaders(w)
	if !h.guard.Authorize(w, r) {
		return
	}
	switch route {
	case "list":
		h.page(w, r, listHTML)
	case "listdata":
		h.listData(w, r)
	case "run":
		h.page(w, r, runHTML)
	case "rundata":
		h.runData(w, r, id)
	case "transcript":
		h.transcript(w, r, id, n)
	}
}

// parsePath maps a URL path to a route.
func parsePath(p string) (route, id string, n int, ok bool) {
	switch p {
	case ListPath, prefix:
		return "list", "", 0, true
	case DataPath:
		return "listdata", "", 0, true
	}
	rest := strings.TrimPrefix(p, prefix)
	if rest == p {
		return "", "", 0, false
	}
	parts := strings.Split(rest, "/")
	if !diag.ValidID(parts[0]) {
		return "", "", 0, false
	}
	id = parts[0]
	switch {
	case len(parts) == 1 || (len(parts) == 2 && parts[1] == ""):
		return "run", id, 0, true
	case len(parts) == 2 && parts[1] == "data.json":
		return "rundata", id, 0, true
	case len(parts) == 2 && strings.HasPrefix(parts[1], "claude-") && strings.HasSuffix(parts[1], ".jsonl"):
		v, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(parts[1], "claude-"), ".jsonl"))
		if err != nil || v < 1 || v > 1000 || parts[1] != diag.TranscriptName(v) {
			return "", "", 0, false
		}
		return "transcript", id, v, true
	}
	return "", "", 0, false
}

//go:embed list.html
var listHTML string

//go:embed run.html
var runHTML string

// diagCSS is the look of both pages (the list, the run detail of the page
// and of the side panel); not part of webui.Assets, the other pages do not
// need it.
//
//go:embed diag.css
var diagCSS string

// kindLabel is the display name of a kind. The /runs kind filter and the
// labels in the pages are generated from core.AllJobKinds(), so a new kind
// shows up there automatically.
func kindLabel(k core.JobKind) string { return webui.KindLabel(k) }

// kindOptionsHTML is the <option>s of the kind filter, built once from
// core.AllJobKinds().
var kindOptionsHTML = buildKindOptions()

func buildKindOptions() string {
	var opts strings.Builder
	for _, k := range core.AllJobKinds() {
		fmt.Fprintf(&opts, `<option value="%s">%s</option>`, html.EscapeString(string(k)), html.EscapeString(kindLabel(k)))
	}
	return opts.String()
}

func (h *handler) page(w http.ResponseWriter, r *http.Request, tmpl string) {
	nonce := usageweb.Nonce()
	usageweb.SetChartPageCSP(w, nonce)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	css, js := webui.Assets()
	page := strings.NewReplacer("{{COMMON_CSS}}", css, "{{COMMON_JS}}", js, "{{FILTERS}}", webassets.FiltersPath,
		"{{HEADER}}", h.o.Nav.Header(r.Context(), webui.SectionRuns), "{{RUNDETAIL}}", webassets.RunDetailPath, "{{DIAG_CSS}}", diagCSS, "{{KIND_OPTIONS}}", kindOptionsHTML).Replace(tmpl)
	_, _ = io.WriteString(w, strings.ReplaceAll(page, "{{NONCE}}", nonce))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

// Row is one run in data.json.
type Row struct {
	ID          string    `json:"id"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
	Kind        string    `json:"kind"`
	ProjectPath string    `json:"project_path"`
	MRIID       int64     `json:"mr_iid"`
	MRTitle     string    `json:"mr_title,omitempty"`
	MRURL       string    `json:"mr_url,omitempty"`
	Username    string    `json:"username,omitempty"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
	Error       string    `json:"error,omitempty"`
	RetryAt     time.Time `json:"retry_at,omitzero"`
	Attempt     int       `json:"attempt"`
	DurationMS  int64     `json:"duration_ms"`
	ClaudeRuns  int       `json:"claude_runs"`
	Tokens      int64     `json:"tokens"`
	CostUSD     float64   `json:"cost_usd"`
	SizeBytes   int64     `json:"size_bytes"`
}

// ListData is the /runs/data.json document.
type ListData struct {
	Since     time.Time `json:"since"`
	Until     time.Time `json:"until"`
	Retention string    `json:"retention"`
	Filter    Filter    `json:"filter"`
	Truncated bool      `json:"truncated"`
	Runs      []Row     `json:"runs"`
	// Queue is what the queue holds right now, whatever the filters
	// (nil: no queue source, or it could not be read).
	Queue *QueueData `json:"queue,omitempty"`
}

// maxQueueItems bounds QueueData.Items (the counts stay exact).
const maxQueueItems = 20

// QueueData is the persisted queue: jobs a handler started (queue_jobs
// with started_at) are "running"; the other jobs and the pending automatic
// reviews are "waiting". A pending automatic review keeps its row while it
// runs, so the page matches it against the running runs of the list
// (status "running", same project and MR) before counting it as waiting.
type QueueData struct {
	Running int         `json:"running"`
	Waiting int         `json:"waiting"`
	Items   []QueueItem `json:"items"`
}

// QueueItem is one queued job: what, where and when, never its text
// (question, thread, note).
type QueueItem struct {
	State       string    `json:"state"` // "running" or "waiting"
	Auto        bool      `json:"auto,omitempty"`
	Kind        string    `json:"kind"`
	ProjectPath string    `json:"project_path,omitempty"`
	MRIID       int64     `json:"mr_iid,omitempty"`
	Username    string    `json:"username,omitempty"`
	EnqueuedAt  time.Time `json:"enqueued_at,omitzero"`
	ReadyAt     time.Time `json:"ready_at,omitzero"`
	StartedAt   time.Time `json:"started_at,omitzero"`
}

// queueData reads the queue source; nil without one or on an error
// (logged: the list itself still answers).
func (h *handler) queueData(ctx context.Context) *QueueData {
	if h.o.Queue == nil {
		return nil
	}
	jobs, err := h.o.Queue.ListJobs(ctx)
	if err != nil {
		h.o.Logger.Error("diag: list queued jobs", "err", err)
		return nil
	}
	pending, err := h.o.Queue.ListPending(ctx)
	if err != nil {
		h.o.Logger.Error("diag: list pending reviews", "err", err)
		return nil
	}
	var running, waiting []QueueItem
	for _, s := range jobs {
		if s.Job.Kind == "" {
			continue // unreadable row, dropped by the next restore
		}
		it := QueueItem{State: "waiting", Kind: string(s.Job.Kind), ProjectPath: s.Job.ProjectPath, MRIID: s.Job.MRIID,
			Username: s.Job.Username, EnqueuedAt: s.Job.EnqueuedAt, ReadyAt: s.ReadyAt, StartedAt: s.StartedAt}
		if !s.StartedAt.IsZero() {
			it.State = "running"
			running = append(running, it)
		} else {
			waiting = append(waiting, it)
		}
	}
	for _, p := range pending {
		waiting = append(waiting, QueueItem{State: "waiting", Auto: true, Kind: string(p.Job.Kind), ProjectPath: p.Job.ProjectPath,
			MRIID: p.Job.MRIID, EnqueuedAt: p.Job.EnqueuedAt, ReadyAt: p.ReadyAt})
	}
	// Running first, the longest first; then waiting, the soonest due first.
	slices.SortStableFunc(running, func(a, b QueueItem) int { return a.StartedAt.Compare(b.StartedAt) })
	slices.SortStableFunc(waiting, func(a, b QueueItem) int { return a.ReadyAt.Compare(b.ReadyAt) })
	d := &QueueData{Running: len(running), Waiting: len(waiting), Items: append(running, waiting...)}
	if len(d.Items) > maxQueueItems {
		d.Items = d.Items[:maxQueueItems]
	}
	if d.Items == nil {
		d.Items = []QueueItem{}
	}
	return d
}

// Filter echoes the applied filters.
type Filter struct {
	Status  string `json:"status,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Project string `json:"project,omitempty"`
	MR      int64  `json:"mr,omitempty"`
	Since   string `json:"since"`
}

var statuses = map[string]bool{diag.StatusRunning: true, diag.StatusOK: true, diag.StatusSkipped: true,
	diag.StatusPostponed: true, diag.StatusFailed: true, diag.StatusInterrupted: true}

// parseFilter reads status, kind, project, mr (IID or "!IID") and since
// (a Go duration, default diag.retention).
func (h *handler) parseFilter(q url.Values) (diag.IndexFilter, Filter, error) {
	now := h.o.Now()
	var f diag.IndexFilter
	var flt Filter
	if s := strings.TrimSpace(q.Get("status")); s != "" {
		if !statuses[s] {
			return f, flt, fmt.Errorf("unknown status %q", s)
		}
		f.Status, flt.Status = s, s
	}
	f.Kind = strings.TrimSpace(q.Get("kind"))
	flt.Kind = f.Kind
	f.ProjectPath = strings.TrimSpace(q.Get("project"))
	flt.Project = f.ProjectPath
	if s := strings.TrimPrefix(strings.TrimSpace(q.Get("mr")), "!"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 {
			return f, flt, fmt.Errorf("bad mr %q", s)
		}
		f.MRIID, flt.MR = v, v
	}
	win := h.o.Retention
	if s := strings.TrimSpace(q.Get("since")); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 || d > 31*24*time.Hour {
			return f, flt, fmt.Errorf("bad since %q (a duration such as 6h or 48h)", s)
		}
		win = d
	}
	f.Since = now.Add(-win)
	flt.Since = shortDuration(win)
	f.Limit = maxRows + 1
	return f, flt, nil
}

func shortDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return d.String()
}

func (h *handler) listData(w http.ResponseWriter, r *http.Request) {
	f, flt, err := h.parseFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := h.o.Reader.List(r.Context(), f)
	if err != nil {
		h.o.Logger.Error("diag: list runs", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d := ListData{Since: f.Since, Until: h.o.Now(), Retention: shortDuration(h.o.Retention), Filter: flt, Runs: make([]Row, 0, len(rows)),
		Queue: h.queueData(r.Context())}
	if len(rows) > maxRows {
		rows, d.Truncated = rows[:maxRows], true
	}
	for _, x := range rows {
		d.Runs = append(d.Runs, Row{ID: x.ID, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt, Kind: x.Kind, ProjectPath: x.ProjectPath,
			MRIID: x.MRIID, MRTitle: x.MRTitle, MRURL: safeURL(x.MRURL), Username: x.Username, Status: x.Status, Reason: x.Reason,
			Error: x.Error, RetryAt: x.RetryAt, Attempt: x.Attempt, DurationMS: x.DurationMS, ClaudeRuns: x.ClaudeRuns,
			Tokens: x.Tokens, CostUSD: x.CostUSD, SizeBytes: x.SizeBytes})
	}
	writeJSON(w, d)
}

// safeURL keeps only http(s) links.
func safeURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return s
}

// RunData is the /runs/<id>/data.json document.
type RunData struct {
	Run    *diag.Record `json:"run"`
	Sizes  []int64      `json:"transcript_sizes"`
	Status string       `json:"status"` // index status (may say "interrupted")
	// TraceURL opens the run's trace (tracing.ui_url); "" = no link.
	TraceURL string `json:"trace_url,omitempty"`
}

// traceIDRe is a W3C trace id (what diag stores; anything else gets no link).
var traceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TraceLink fills the ui_url template with a trace id ("" when either is
// missing or the result is not an http(s) URL).
func TraceLink(tmpl, traceID string) string {
	if tmpl == "" || !traceIDRe.MatchString(traceID) {
		return ""
	}
	return safeURL(strings.ReplaceAll(tmpl, "{trace_id}", traceID))
}

func (h *handler) runData(w http.ResponseWriter, r *http.Request, id string) {
	row, rec, err := h.o.Reader.Get(r.Context(), id)
	if err != nil {
		h.o.Logger.Error("diag: read run", "run", id, "err", err)
		if row == nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if row == nil || rec == nil {
		http.NotFound(w, r)
		return
	}
	rec.Job.MRURL = safeURL(rec.Job.MRURL)
	d := RunData{Run: rec, Status: row.Status, TraceURL: TraceLink(h.o.TraceURL, rec.TraceID)}
	if row.Status == diag.StatusInterrupted && rec.Status == diag.StatusRunning {
		rec.Status, rec.Reason = row.Status, row.Reason
	}
	for i := range rec.Claude {
		var size int64
		if fi, err := os.Stat(h.o.Reader.TranscriptPath(row, i+1)); err == nil {
			size = fi.Size()
		}
		d.Sizes = append(d.Sizes, size)
	}
	writeJSON(w, d)
}

func (h *handler) transcript(w http.ResponseWriter, r *http.Request, id string, n int) {
	row, err := h.o.Reader.Index.GetDiagRun(r.Context(), id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p := h.o.Reader.TranscriptPath(row, n)
	if p == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s"`, id, diag.TranscriptName(n)))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
