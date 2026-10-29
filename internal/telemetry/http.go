package telemetry

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/metrics"
)

// Paths.
const (
	MetricsPath = "/metrics"
	ReadyPath   = "/readyz"
)

// MetricsOptions configure GET /metrics.
type MetricsOptions struct {
	Enabled bool
	// Token, when set, must be sent as "Authorization: Bearer <token>"
	// (scrapers: no query token, no cookie). Empty = open.
	Token  string
	Logger *slog.Logger
}

// MetricsHandler serves the registry in the text format. Disabled -> 404.
func (m *Metrics) MetricsHandler(o MetricsOptions) http.Handler {
	var want []byte
	if o.Token != "" {
		s := sha256.Sum256([]byte(o.Token))
		want = s[:]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !o.Enabled || m == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if want != nil && !bearerOK(r, want) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="reviewbot-metrics"`)
			http.Error(w, "unauthorized: send Authorization: Bearer <METRICS_TOKEN>", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", metrics.ContentType)
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		_ = m.Registry.WriteText(w)
	})
}

func bearerOK(r *http.Request, want []byte) bool {
	auth := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(auth) <= len(p) || !strings.EqualFold(auth[:len(p)], p) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(auth[len(p):])))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// RegisterMetrics mounts /metrics and logs how it is protected.
func (m *Metrics) RegisterMetrics(mux *http.ServeMux, o MetricsOptions) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	mux.Handle(MetricsPath, m.MetricsHandler(o))
	switch {
	case !o.Enabled:
		o.Logger.Info("metrics endpoint disabled (metrics.enabled: false)")
	case o.Token == "":
		o.Logger.Info("metrics endpoint " + MetricsPath + " is open without authentication (no user, project or MR " +
			"labels; set METRICS_TOKEN or metrics.token to require Authorization: Bearer)")
	default:
		o.Logger.Info("metrics endpoint enabled with bearer token", "path", MetricsPath)
	}
}

// ---------------------------------------------------------------------------
// Readiness
// ---------------------------------------------------------------------------

// Check is one readiness check. Run must honour ctx and must never put
// secrets in its error.
type Check struct {
	Name string
	Run  func(ctx context.Context) error
	// CacheFor keeps a result (ok or not) this long; 0 = check every time.
	CacheFor time.Duration
	// Timeout bounds one run (default 5s).
	Timeout time.Duration
}

// CheckResult is one check in the /readyz answer.
type CheckResult struct {
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

// ReadyReport is the /readyz answer.
type ReadyReport struct {
	Status string        `json:"status"` // ok | fail
	Checks []CheckResult `json:"checks"`
}

// Readiness runs the checks with per-check caching. Safe for concurrent use;
// concurrent callers of an expired check share one run.
type Readiness struct {
	checks  []*cachedCheck
	now     func() time.Time
	secrets []string
	gauge   *metrics.GaugeVec
}

type cachedCheck struct {
	Check
	mu  sync.Mutex
	at  time.Time
	res CheckResult
	has bool
}

// ReadinessOptions configure NewReadiness.
type ReadinessOptions struct {
	Checks []Check
	// Secrets are literal values removed from check errors (defence in
	// depth: checks should not include them anyway).
	Secrets []string
	Now     func() time.Time
}

// NewReadiness builds the checker. With m non-nil every Report also sets
// reviewbot_readiness_check_ok, and every scrape refreshes it (cached).
func (m *Metrics) NewReadiness(o ReadinessOptions) *Readiness {
	rd := &Readiness{now: o.Now}
	if rd.now == nil {
		rd.now = time.Now
	}
	for _, s := range o.Secrets {
		if s = strings.TrimSpace(s); len(s) >= 4 {
			rd.secrets = append(rd.secrets, s)
		}
	}
	// Longest first: a secret that contains another one must be removed
	// whole, not leave its tail behind after the shorter one is replaced.
	sort.SliceStable(rd.secrets, func(i, j int) bool { return len(rd.secrets[i]) > len(rd.secrets[j]) })
	for _, c := range o.Checks {
		if c.Timeout <= 0 {
			c.Timeout = 5 * time.Second
		}
		rd.checks = append(rd.checks, &cachedCheck{Check: c})
	}
	if m != nil {
		rd.gauge = m.ReadinessCheckOK
		// Scrapes refresh the gauge through the same cache; the short bound
		// keeps /metrics within a typical 10s scrape timeout.
		m.Registry.OnCollect(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			rd.Report(ctx)
		})
	}
	return rd
}

// Report runs (or reuses) every check, in parallel.
func (rd *Readiness) Report(ctx context.Context) ReadyReport {
	out := ReadyReport{Status: "ok", Checks: make([]CheckResult, len(rd.checks))}
	var wg sync.WaitGroup
	for i, c := range rd.checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out.Checks[i] = rd.run(ctx, c)
		}()
	}
	wg.Wait()
	for _, r := range out.Checks {
		if !r.OK {
			out.Status = "fail"
		}
		if rd.gauge != nil {
			v := 0.0
			if r.OK {
				v = 1
			}
			rd.gauge.With(r.Name).Set(v)
		}
	}
	return out
}

func (rd *Readiness) run(ctx context.Context, c *cachedCheck) CheckResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := rd.now()
	if c.has && c.CacheFor > 0 && now.Sub(c.at) < c.CacheFor {
		return c.res
	}
	cctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	start := time.Now()
	err := safeRun(cctx, c.Run)
	res := CheckResult{Name: c.Name, OK: err == nil, LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = rd.scrub(err.Error())
		if ctx.Err() != nil {
			// The caller gave up (probe client gone, the scrape's shorter
			// bound): this says nothing about the dependency, so it must not
			// be served from the cache to the next caller.
			return res
		}
	}
	c.at, c.res, c.has = now, res, true
	return res
}

func safeRun(ctx context.Context, f func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = errors.New("check panicked")
		}
	}()
	return f(ctx)
}

func (rd *Readiness) scrub(s string) string {
	for _, sec := range rd.secrets {
		s = strings.ReplaceAll(s, sec, diag.Redacted)
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// ServeHTTP answers /readyz: 200 when every check is ok, 503 otherwise.
func (rd *Readiness) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rep := rd.Report(r.Context())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if rep.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(rep)
}

// ---------------------------------------------------------------------------
// Standard checks
// ---------------------------------------------------------------------------

// GitLabCheck: GET /user with the bot token (cached 30s).
func GitLabCheck(currentUser func(ctx context.Context) (int64, string, error)) Check {
	return Check{Name: "gitlab", CacheFor: 30 * time.Second, Timeout: 10 * time.Second, Run: func(ctx context.Context) error {
		_, _, err := currentUser(ctx)
		return err
	}}
}

// StoreCheck: SQLite ping (not cached).
func StoreCheck(ping func(ctx context.Context) error) Check {
	return Check{Name: "store", Timeout: 5 * time.Second, Run: ping}
}

// DockerCheck: `docker version --format '{{.Server.Version}}'` with a 5s
// timeout (cached 30s). Only for sandbox.mode docker. run is the command
// runner (nil = os/exec); it returns combined output.
func DockerCheck(dockerBin string, run func(ctx context.Context, name string, args ...string) ([]byte, error)) Check {
	if dockerBin == "" {
		dockerBin = "docker"
	}
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	return Check{Name: "docker", CacheFor: 30 * time.Second, Timeout: 5 * time.Second, Run: func(ctx context.Context) error {
		out, err := run(ctx, dockerBin, "version", "--format", "{{.Server.Version}}")
		v := strings.TrimSpace(string(out))
		if err != nil {
			if ctx.Err() != nil {
				return errors.New("docker version: timeout")
			}
			if v != "" {
				return errors.New("docker version: " + lastLine(v))
			}
			return errors.New("docker version: " + err.Error())
		}
		if v == "" {
			return errors.New("docker version: empty server version (daemon unreachable?)")
		}
		return nil
	}}
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
