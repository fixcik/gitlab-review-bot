// Package mapweb serves the system map page:
//
//	GET /map            graph of repositories, contracts, unresolved calls, scan status
//	GET /map/data.json  the same data as JSON
//
// The page is self-contained HTML (inline CSS/JS under a nonce CSP plus the
// filter bar and the graph renderer from internal/webassets: filters.js,
// map.js) and loads everything from data.json. The relations the page shows
// (which endpoint a call reaches, who calls an endpoint, calls linked to a
// repository without such an endpoint) are computed here with the matching
// rules of sysmap (Index.Relations), not in the browser. Every string there comes from repositories (paths, raw call
// expressions, event names) and is untrusted: the page inserts it as text
// only. Access works like /usage (usageweb.Guard): optional token, enabled
// flag, startup warning when open.
package mapweb

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/usageweb"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// Paths.
const (
	PagePath = "/map"
	DataPath = "/map/data.json"
)

const cookieName = "reviewbot_map"

// Caps of data.json lists.
const (
	maxContracts  = 5000
	maxUnresolved = 2000
)

// Source is what the page reads (*sysmap.Map).
type Source interface {
	Index() *sysmap.Index
	RunStats(ctx context.Context, since time.Time) (sysmap.RunStats, error)
	MaxRunsPerHour() int
	HintsList() [][2]string
}

// Options configure the handler.
type Options struct {
	// Nav is the shared header (switched-off pages, problems badge).
	Nav webui.Nav
	// Map is nil when the system map is off; the page then answers 404.
	Map Source
	// Enabled=false makes both endpoints answer 404.
	Enabled bool
	// Token, when non-empty, is required (see usageweb.Guard).
	Token string
	// GitLabURL is used for repository and file links.
	GitLabURL string
	// MaxHintKey is the longest key review.sysmap.hints accepts
	// (config.MaxSysmapHintKeyLen); 0 = no limit known.
	MaxHintKey int
	Now        func() time.Time
	Logger     *slog.Logger
}

type handler struct {
	o     Options
	guard *usageweb.Guard
}

// New returns the handler (mount it on PagePath and DataPath).
func New(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	o.GitLabURL = strings.TrimRight(o.GitLabURL, "/")
	return &handler{o: o, guard: usageweb.NewGuard(o.Token, cookieName, PagePath, PagePath, "MAP_TOKEN")}
}

// Register mounts the handler and logs how the page is protected.
func Register(mux *http.ServeMux, o Options) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	h := New(o)
	mux.Handle(PagePath, h)
	mux.Handle(DataPath, h)
	switch {
	case !o.Enabled || o.Map == nil:
		o.Logger.Info("system map page disabled (review.sysmap off or context repositories off)")
	case o.Token == "":
		o.Logger.Warn("system map page " + PagePath + " is open WITHOUT authentication: anyone who can reach the bot's port " +
			"sees the repositories, their endpoints and call sites. To protect it set MAP_TOKEN (or review.sysmap.token, " +
			"or USAGE_TOKEN) and open " + PagePath + "?token=<token> once")
	default:
		o.Logger.Info("system map page enabled with token", "path", PagePath)
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.o.Enabled || h.o.Map == nil || (r.URL.Path != PagePath && r.URL.Path != DataPath) {
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
	if r.URL.Path == DataPath {
		h.serveData(w, r)
		return
	}
	h.servePage(w, r)
}

//go:embed map.html
var pageHTML string

// pageCSS is the look of the page (graph, action list, tables), inlined
// after the shared CSS.
//
//go:embed map.css
var pageCSS string

func (h *handler) servePage(w http.ResponseWriter, r *http.Request) {
	nonce := usageweb.Nonce()
	usageweb.SetChartPageCSP(w, nonce)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	css, js := webui.Assets()
	page := strings.NewReplacer("{{HEADER}}", h.o.Nav.Header(r.Context(), webui.SectionMap), "{{COMMON_CSS}}", css, "{{MAP_CSS}}", pageCSS, "{{COMMON_JS}}", js, "{{ELK}}", webassets.ElkPath, "{{FILTERS}}", webassets.FiltersPath,
		"{{MAPJS}}", webassets.MapPath).Replace(pageHTML)
	_, _ = io.WriteString(w, strings.ReplaceAll(page, "{{NONCE}}", nonce))
}

// ---------------------------------------------------------------------------
// data.json
// ---------------------------------------------------------------------------

// Repo is one repository row.
type Repo struct {
	Path       string    `json:"path"`
	URL        string    `json:"url,omitempty"`
	SHA        string    `json:"sha"`
	Branch     string    `json:"branch"`
	ScannedAt  time.Time `json:"scanned_at"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	Candidates int       `json:"candidates"`
	Specs      int       `json:"specs"`
	Pending    int       `json:"pending"`
	Capped     int       `json:"capped"`
	TooLarge   int       `json:"too_large"`
	FileErrors int       `json:"file_errors"`
	Provides   int       `json:"provides"`
	Consumes   int       `json:"consumes"`
	Unresolved int       `json:"unresolved"`
	// External: calls to third-party systems (not unresolved).
	External int `json:"external"`
	// ByRule: calls linked by a topology rule (neither unresolved nor
	// matched in code).
	ByRule int `json:"by_rule"`
	// Endpoints: distinct endpoints the repository provides (an OpenAPI
	// and a code description of one route count once); IdleEndpoints:
	// those no other repository calls.
	Endpoints     int `json:"endpoints"`
	IdleEndpoints int `json:"idle_endpoints"`
	// NoEndpoint: resolved calls whose target has no such endpoint.
	NoEndpoint int `json:"no_endpoint"`
	// InMap is false for repositories that only appear as a hint target or
	// edge end (not scanned).
	InMap bool `json:"in_map"`
	// SpecOnly: the repository provides endpoints only through
	// OpenAPI/Swagger files, no route in code (e.g. a frontend keeping the
	// backend's swagger.json).
	SpecOnly bool `json:"spec_only"`

	codeRoutes int // provider entries from code
}

// Contract is one entry with its matching result.
type Contract struct {
	Repo       string   `json:"repo"`
	Side       string   `json:"side"`
	Protocol   string   `json:"protocol"`
	Method     string   `json:"method,omitempty"`
	Path       string   `json:"path,omitempty"`
	Event      string   `json:"event,omitempty"`
	Raw        string   `json:"raw,omitempty"`
	Base       string   `json:"base,omitempty"`
	File       string   `json:"file"`
	Line       int      `json:"line"`
	URL        string   `json:"url,omitempty"`
	Note       string   `json:"note,omitempty"`
	Source     string   `json:"source"`
	Status     string   `json:"status,omitempty"`
	Target     string   `json:"target,omitempty"`
	Via        string   `json:"via,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	// ID is the position in Data.Contracts (stable: the list is the first
	// entries of the index); Lands, Endpoint.Contracts and Endpoint.Callers
	// refer to it and only list contracts that are in the list.
	ID int `json:"id"`
	// Lands (calls): the provider contracts the call reaches, of its target
	// or, when unresolved, of its candidates.
	Lands []int `json:"lands,omitempty"`
	// NoEndpoint (calls): linked to a repository (mostly by a hint) that
	// provides no such endpoint.
	NoEndpoint bool `json:"no_endpoint,omitempty"`
	// ExternalHost / ExternalService (status "external"): the third-party
	// system the call goes to.
	ExternalHost    string `json:"external_host,omitempty"`
	ExternalService string `json:"external_service,omitempty"`
}

// ExternalDep is one third-party system the repositories call: its host,
// the callers and the call sites (capped like Unresolved over all groups).
type ExternalDep struct {
	Host string `json:"host"`
	// Service: the first non-empty name the model gave the host.
	Service string         `json:"service,omitempty"`
	Total   int            `json:"total"`
	Repos   []ExternalRepo `json:"repos"`
	Sites   []Contract     `json:"sites"`
}

// ExternalRepo counts the calls of one repository to an external system.
type ExternalRepo struct {
	Repo  string `json:"repo"`
	Count int    `json:"count"`
}

// Endpoint is one endpoint of a repository: the provider contracts that
// describe it (OpenAPI and code merged) and the calls of other
// repositories that reach it.
type Endpoint struct {
	Repo      string `json:"repo"`
	Protocol  string `json:"protocol"`
	Method    string `json:"method,omitempty"`
	Path      string `json:"path,omitempty"`
	Event     string `json:"event,omitempty"`
	Contracts []int  `json:"contracts"`
	Callers   []int  `json:"callers"`
	// CallersTotal counts the callers before the contracts cap.
	CallersTotal int `json:"callers_total"`
	// SpecOnly: only a spec describes the endpoint and its repository serves
	// no route in code (Repo.SpecOnly).
	SpecOnly bool `json:"spec_only,omitempty"`
}

// TopologyRule is one review.sysmap.topology rule and the calls it linked.
type TopologyRule struct {
	From string   `json:"from"`
	To   string   `json:"to"`
	Via  []string `json:"via"`
	Hits int      `json:"hits"`
}

// Hint is one configured hint.
type Hint struct {
	Key  string `json:"key"`
	Repo string `json:"repo"`
}

// Cost summarizes the extraction runs.
type Cost struct {
	MaxRunsPerHour int     `json:"max_runs_per_hour"`
	RunsLastHour   int     `json:"runs_last_hour"`
	Runs24h        int     `json:"runs_24h"`
	Failed24h      int     `json:"failed_24h"`
	Runs30d        int     `json:"runs_30d"`
	Failed30d      int     `json:"failed_30d"`
	CostUSD30d     float64 `json:"cost_usd_30d"`
	Tokens30d      int64   `json:"tokens_30d"`
	CostUSDAll     float64 `json:"cost_usd_all"`
	RunsAll        int     `json:"runs_all"`
}

// Data is the data.json document.
type Data struct {
	GeneratedAt time.Time     `json:"generated_at"`
	BuiltAt     time.Time     `json:"built_at"`
	GitLabURL   string        `json:"gitlab_url,omitempty"`
	Repos       []Repo        `json:"repos"`
	Edges       []sysmap.Edge `json:"edges"`
	Contracts   []Contract    `json:"contracts"`
	// ContractsTotal / UnresolvedTotal count before the caps.
	ContractsTotal  int        `json:"contracts_total"`
	Unresolved      []Contract `json:"unresolved"`
	UnresolvedTotal int        `json:"unresolved_total"`
	Hints           []Hint     `json:"hints"`
	// Topology: the configured topology rules with their hits.
	Topology []TopologyRule `json:"topology"`
	Cost     Cost           `json:"cost"`
	// Endpoints: the endpoints whose first contract is in Contracts.
	Endpoints []Endpoint `json:"endpoints"`
	// NoEndpoint: resolved calls whose target has no such endpoint
	// (capped like Unresolved; NoEndpointTotal before the cap).
	NoEndpoint      []Contract `json:"no_endpoint"`
	NoEndpointTotal int        `json:"no_endpoint_total"`
	// MaxHintKey is the longest key review.sysmap.hints accepts (the page
	// does not offer a snippet for a longer base); 0 = unknown.
	MaxHintKey int `json:"max_hint_key"`
	// External: calls to third-party systems grouped by host, most called
	// first; ExternalTotal counts the calls before the cap of the sites.
	External      []ExternalDep `json:"external"`
	ExternalTotal int           `json:"external_total"`
}

func (h *handler) serveData(w http.ResponseWriter, r *http.Request) {
	d, err := h.collect(r.Context())
	if err != nil {
		h.o.Logger.Error("map: collect", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(d)
}

func (h *handler) collect(ctx context.Context) (*Data, error) {
	now := h.o.Now()
	d := &Data{GeneratedAt: now, GitLabURL: h.o.GitLabURL, Repos: []Repo{}, Edges: []sysmap.Edge{}, Contracts: []Contract{},
		Unresolved: []Contract{}, Hints: []Hint{}, Endpoints: []Endpoint{}, NoEndpoint: []Contract{}, MaxHintKey: h.o.MaxHintKey,
		External: []ExternalDep{}, Topology: []TopologyRule{}}
	x := h.o.Map.Index()
	for _, kv := range h.o.Map.HintsList() {
		d.Hints = append(d.Hints, Hint{Key: kv[0], Repo: kv[1]})
	}
	cost, err := h.cost(ctx, now)
	if err != nil {
		return nil, err
	}
	d.Cost = cost
	if x == nil {
		return d, nil
	}
	d.BuiltAt = x.BuiltAt
	shas := map[string]string{}
	repos := map[string]*Repo{}
	var order []string
	addRepo := func(path string) *Repo {
		k := strings.ToLower(path)
		if r, ok := repos[k]; ok {
			return r
		}
		r := &Repo{Path: path, URL: h.projectURL(path)}
		repos[k] = r
		order = append(order, k)
		return r
	}
	for _, s := range x.Repos {
		r := addRepo(s.Path)
		r.InMap, r.SHA, r.Branch, r.ScannedAt, r.Status, r.Error = true, s.SHA, s.Branch, s.ScannedAt, s.Status, s.Error
		r.Candidates, r.Specs, r.Pending, r.Capped, r.TooLarge = s.Candidates, s.Specs, s.Pending, s.Capped, s.TooLarge
		r.FileErrors = x.FileErrors[s.Path]
		shas[strings.ToLower(s.Path)] = s.SHA
	}
	rel := x.Relations()
	ext := &externals{}
	listed := func(ids []int) []int {
		out := []int{}
		for _, i := range ids {
			if i < maxContracts {
				out = append(out, i)
			}
		}
		return out
	}
	for i, e := range x.Entries {
		r := addRepo(e.Repo)
		if e.Side == sysmap.Provides {
			r.Provides++
			if e.Source != sysmap.SourceOpenAPI {
				r.codeRoutes++
			}
		} else {
			r.Consumes++
		}
		ct := Contract{Repo: e.Repo, Side: string(e.Side), Protocol: string(e.Protocol), Method: e.Method, Path: e.Path, Event: e.Event,
			Raw: e.Raw, Base: e.Base, File: e.File, Line: e.Line, Note: e.Note, Source: e.Source, Status: e.Status, Target: e.Target,
			Via: e.Via, Reason: e.Reason, Candidates: e.Candidates, URL: h.blobURL(e.Repo, shas[strings.ToLower(e.Repo)], e.File, e.Line),
			ID: i, NoEndpoint: rel.NoEndpoint[i], ExternalHost: e.ExternalHost, ExternalService: e.ExternalService}
		if len(rel.Lands[i]) > 0 {
			ct.Lands = listed(rel.Lands[i])
		}
		d.ContractsTotal++
		if len(d.Contracts) < maxContracts {
			d.Contracts = append(d.Contracts, ct)
		}
		if e.Status == sysmap.Unresolved {
			r.Unresolved++
			d.UnresolvedTotal++
			if len(d.Unresolved) < maxUnresolved {
				d.Unresolved = append(d.Unresolved, ct)
			}
		}
		if e.Status == sysmap.Topology {
			r.ByRule++
		}
		if e.Status == sysmap.External {
			r.External++
			ext.add(ct)
		}
		if ct.NoEndpoint {
			r.NoEndpoint++
			d.NoEndpointTotal++
			if len(d.NoEndpoint) < maxUnresolved {
				d.NoEndpoint = append(d.NoEndpoint, ct)
			}
		}
	}
	for _, r := range repos {
		r.SpecOnly = r.Provides > 0 && r.codeRoutes == 0
	}
	for _, g := range rel.Endpoints {
		r := addRepo(g.Repo)
		r.Endpoints++
		if len(g.Callers) == 0 {
			r.IdleEndpoints++
		}
		if g.Entries[0] >= maxContracts {
			continue
		}
		e := x.Entries[g.Entries[0]]
		d.Endpoints = append(d.Endpoints, Endpoint{Repo: e.Repo, Protocol: string(e.Protocol), Method: e.Method, Path: e.Path, Event: e.Event,
			Contracts: listed(g.Entries), Callers: listed(g.Callers), CallersTotal: len(g.Callers), SpecOnly: r.SpecOnly})
	}
	d.External, d.ExternalTotal = ext.list()
	for i, tr := range x.Topology {
		via := []string{}
		for _, p := range tr.Via {
			via = append(via, string(p))
		}
		d.Topology = append(d.Topology, TopologyRule{From: tr.From, To: tr.To, Via: via, Hits: x.TopologyHits[i]})
	}
	for _, ed := range x.Edges {
		addRepo(ed.From)
		addRepo(ed.To)
		d.Edges = append(d.Edges, ed)
	}
	sort.Strings(order)
	for _, k := range order {
		d.Repos = append(d.Repos, *repos[k])
	}
	return d, nil
}

// externals groups the external calls by host.
type externals struct {
	groups map[string]*ExternalDep
	repos  map[string]map[string]int // host -> repo -> calls
	total  int
}

func (g *externals) add(c Contract) {
	if g.groups == nil {
		g.groups, g.repos = map[string]*ExternalDep{}, map[string]map[string]int{}
	}
	// A configured external system (review.sysmap.externals) may have no
	// host: it groups by its name.
	key := c.ExternalHost
	if key == "" {
		key = "\x00" + c.ExternalService
	}
	gr := g.groups[key]
	if gr == nil {
		gr = &ExternalDep{Host: c.ExternalHost, Service: c.ExternalService, Sites: []Contract{}}
		g.groups[key], g.repos[key] = gr, map[string]int{}
	}
	if gr.Service == "" {
		gr.Service = c.ExternalService
	}
	gr.Total++
	gr.Sites = append(gr.Sites, c)
	g.repos[key][c.Repo]++
	g.total++
}

// list returns the groups, most called first, and the number of calls; the
// sites are the first maxUnresolved calls in that order.
func (g *externals) list() ([]ExternalDep, int) {
	out := []ExternalDep{}
	for key, gr := range g.groups {
		for r, n := range g.repos[key] {
			gr.Repos = append(gr.Repos, ExternalRepo{Repo: r, Count: n})
		}
		sort.Slice(gr.Repos, func(i, j int) bool { return gr.Repos[i].Repo < gr.Repos[j].Repo })
		out = append(out, *gr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Service < out[j].Service
	})
	left := maxUnresolved
	for i := range out {
		n := min(len(out[i].Sites), left)
		out[i].Sites, left = out[i].Sites[:n], left-n
	}
	return out, g.total
}

// cost reads the extraction run stats of the last hour, day, 30 days and
// all time.
func (h *handler) cost(ctx context.Context, now time.Time) (Cost, error) {
	c := Cost{MaxRunsPerHour: h.o.Map.MaxRunsPerHour()}
	for _, p := range []struct {
		since      time.Time
		runs, fail *int
		cost       *float64
		tokens     *int64
	}{
		{now.Add(-time.Hour), &c.RunsLastHour, nil, nil, nil},
		{now.Add(-24 * time.Hour), &c.Runs24h, &c.Failed24h, nil, nil},
		{now.Add(-30 * 24 * time.Hour), &c.Runs30d, &c.Failed30d, &c.CostUSD30d, &c.Tokens30d},
		{time.Time{}, &c.RunsAll, nil, &c.CostUSDAll, nil},
	} {
		st, err := h.o.Map.RunStats(ctx, p.since)
		if err != nil {
			return Cost{}, err
		}
		*p.runs = st.Runs
		if p.fail != nil {
			*p.fail = st.Failed
		}
		if p.cost != nil {
			*p.cost = st.CostUSD
		}
		if p.tokens != nil {
			*p.tokens = st.Tokens
		}
	}
	return c, nil
}

func pathSegments(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func (h *handler) projectURL(path string) string {
	if h.o.GitLabURL == "" || path == "" {
		return ""
	}
	return h.o.GitLabURL + "/" + pathSegments(path)
}

// blobURL links a file line at the scanned commit ("" when unknown).
func (h *handler) blobURL(repo, sha, file string, line int) string {
	if h.o.GitLabURL == "" || sha == "" || file == "" {
		return ""
	}
	for _, seg := range strings.Split(file, "/") {
		if seg == ".." || seg == "." {
			return ""
		}
	}
	u := fmt.Sprintf("%s/%s/-/blob/%s/%s", h.o.GitLabURL, pathSegments(repo), url.PathEscape(sha), pathSegments(file))
	if line > 0 {
		u += fmt.Sprintf("#L%d", line)
	}
	return u
}
