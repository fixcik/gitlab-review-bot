// Package setupweb serves /setup: the result of `reviewbot doctor` on the
// running bot's config, read-only (no inputs, nothing is written), and the
// same report as /setup/data.json. Access works like /usage
// (usageweb.Guard, the same optional token).
//
// The checks are GETs to GitLab, `docker ... inspect` and a CONNECT through
// the egress proxy. A report is kept for TTL; «Обновить» (?refresh=1) runs
// them again, but not more often than MinRefresh, and never two runs at a
// time, so a page holder cannot hammer GitLab or the docker daemon. A run
// has its own timeout and does not depend on the request.
//
// The page is rendered on the server; «Обновить» is a link (the CSP allows
// no form), so no page script is needed.
package setupweb

import (
	"context"
	_ "embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/doctor"
	"github.com/fixcik/gitlab-review-bot/internal/usageweb"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// Paths served.
const (
	PagePath = "/setup"
	DataPath = "/setup/data.json"
)

const cookieName = "reviewbot_setup"

// Options configure the handler.
type Options struct {
	Nav webui.Nav
	// Token, when non-empty, is required (see usageweb.Guard).
	Token string
	// Run runs the checks (cmd: doctor.Run over the config's inputs).
	Run func(ctx context.Context) doctor.Report
	// TTL: how long a report is shown without a rerun (default 1 min);
	// MinRefresh: the shortest interval between runs (default 15 s);
	// Timeout: of one run (default 60 s).
	TTL, MinRefresh, Timeout time.Duration
	Now                      func() time.Time
	Logger                   *slog.Logger
}

type handler struct {
	o     Options
	guard *usageweb.Guard

	mu   sync.Mutex // one run at a time; guards last
	last *doctor.Report
}

// New returns the handler of PagePath and DataPath.
func New(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.TTL <= 0 {
		o.TTL = time.Minute
	}
	if o.MinRefresh <= 0 {
		o.MinRefresh = 15 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	return &handler{o: o, guard: usageweb.NewGuard(o.Token, cookieName, PagePath, PagePath, "USAGE_TOKEN")}
}

// Register mounts the page and its data.
func Register(mux *http.ServeMux, o Options) {
	h := New(o)
	mux.Handle(PagePath, h)
	mux.Handle(DataPath, h)
}

// report returns the cached report or runs the checks (refresh: the user
// asked, still at most once per MinRefresh).
func (h *handler) report(refresh bool) doctor.Report {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.o.Now()
	if h.last != nil {
		age := now.Sub(h.last.At)
		if age < h.o.MinRefresh || (!refresh && age < h.o.TTL) {
			return *h.last
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.o.Timeout)
	defer cancel()
	r := h.o.Run(ctx)
	r.At = now
	h.last = &r
	return r
}

//go:embed page.html
var pageHTML string

//go:embed setup.css
var pageCSS string

var pageTmpl = template.Must(template.New("setup").Parse(pageHTML))

// Row is one check on the page.
type Row struct {
	doctor.Result
	Mark, Class string
}

// Group is the checks of one group (GitLab, Docker, ...), in run order.
type Group struct {
	Name string
	Rows []Row
}

type pageData struct {
	Nonce              string
	CommonCSS          template.CSS
	CommonJS           template.JS
	Header             template.HTML
	Groups             []Group
	OK, Fails, Warns   int
	At                 string
	FailWord, WarnWord string
}

var marks = map[doctor.Status][2]string{
	doctor.OK: {"✓", "ok"}, doctor.Warn: {"!", "warn"}, doctor.Fail: {"✕", "fail"}, doctor.Skip: {"·", "skip"},
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	usageweb.SecurityHeaders(w)
	if !h.guard.Authorize(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	rep := h.report(r.URL.Query().Get("refresh") == "1")
	if r.URL.Path == DataPath {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(rep)
		return
	}
	nonce := usageweb.Nonce()
	css, js := webui.Assets()
	d := pageData{Nonce: nonce, CommonCSS: template.CSS(css + "\n" + pageCSS), CommonJS: template.JS(js),
		Header: template.HTML(h.o.Nav.Header(r.Context(), webui.SectionSetup)),
		Fails:  rep.Fails, Warns: rep.Warns, At: rep.At.Local().Format("15:04:05"),
		FailWord: plural(rep.Fails, "ошибка", "ошибки", "ошибок"), WarnWord: plural(rep.Warns, "предупреждение", "предупреждения", "предупреждений")}
	for _, x := range rep.Results {
		m := marks[x.Status]
		if n := len(d.Groups); n == 0 || d.Groups[n-1].Name != x.Group {
			d.Groups = append(d.Groups, Group{Name: x.Group})
		}
		g := &d.Groups[len(d.Groups)-1]
		g.Rows = append(g.Rows, Row{Result: x, Mark: m[0], Class: m[1]})
		if x.Status == doctor.OK {
			d.OK++
		}
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+
		"'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	if err := pageTmpl.Execute(w, d); err != nil {
		h.o.Logger.Error("setup: render", "err", err)
	}
}

func plural(n int, one, few, many string) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 10 || n100 >= 20):
		return few
	}
	return many
}
