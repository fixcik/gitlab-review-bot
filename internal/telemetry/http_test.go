package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/metrics"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/webhook"
)

func get(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestMetricsEndpointAccess(t *testing.T) {
	m := New("v")
	mux := http.NewServeMux()
	m.RegisterMetrics(mux, MetricsOptions{Enabled: true})
	w := get(mux, "/metrics")
	if w.Code != 200 || w.Header().Get("Content-Type") != metrics.ContentType ||
		!strings.Contains(w.Body.String(), "# TYPE reviewbot_jobs_total counter") {
		t.Fatalf("open: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}

	h := m.MetricsHandler(MetricsOptions{Enabled: true, Token: "s3cret-metrics"})
	for name, hdr := range map[string][]string{
		"none":          nil,
		"wrong":         {"Authorization", "Bearer nope"},
		"basic":         {"Authorization", "Basic czNjcmV0LW1ldHJpY3M="},
		"query ignored": nil,
	} {
		path := "/metrics"
		if name == "query ignored" {
			path += "?token=s3cret-metrics"
		}
		if w := get(h, path, hdr...); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := get(h, "/metrics", "Cookie", "token=s3cret-metrics"); w.Code != http.StatusUnauthorized {
		t.Errorf("cookie must not work: %d", w.Code)
	}
	if w := get(h, "/metrics", "Authorization", "bearer s3cret-metrics"); w.Code != 200 {
		t.Errorf("bearer: %d", w.Code)
	}

	if w := get(m.MetricsHandler(MetricsOptions{Enabled: false}), "/metrics"); w.Code != http.StatusNotFound {
		t.Errorf("disabled: %d", w.Code)
	}
	var nilM *Metrics
	if w := get(nilM.MetricsHandler(MetricsOptions{Enabled: true}), "/metrics"); w.Code != http.StatusNotFound {
		t.Errorf("nil metrics: %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	w = httptest.NewRecorder()
	m.MetricsHandler(MetricsOptions{Enabled: true}).ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestQueueGaugesSampledAtScrape(t *testing.T) {
	m := New("")
	q := queue.New(queue.Options{Debounce: time.Hour})
	m.ObserveQueue(q)
	q.Enqueue(core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 1})
	q.Enqueue(core.Job{Kind: core.JobAutoReview, ProjectID: 1, MRIID: 2})
	has(t, scrape(t, m),
		`reviewbot_queue_jobs{state="pending"} 2`,
		`reviewbot_queue_jobs{state="running"} 0`,
		`reviewbot_queue_jobs{state="retry_wait"} 0`,
		`reviewbot_queue_draining 0`,
	)
	m.ObserveRestore(core.JobChat, queue.RestoreReplayed)
	m.ObserveRestore(core.JobPause, queue.RestoreExpired)
	has(t, scrape(t, m),
		`reviewbot_queue_restored_total{kind="chat",result="replayed"} 1`,
		`reviewbot_queue_restored_total{kind="other",result="expired"} 1`,
	)
	var nilM *Metrics
	nilM.ObserveRestore(core.JobChat, queue.RestoreQueued) // nil-safe
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func readyz(t *testing.T, rd *Readiness) (int, ReadyReport) {
	t.Helper()
	w := get(rd, "/readyz")
	var rep ReadyReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("json: %v %q", err, w.Body.String())
	}
	return w.Code, rep
}

func TestReadyz(t *testing.T) {
	const secret = "glpat-very-secret-token"
	m := New("")
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}

	var glCalls, dockerCalls atomic.Int32
	var glErr atomic.Value
	glErr.Store(errNil)
	gl := func(ctx context.Context) (int64, string, error) {
		glCalls.Add(1)
		if e := glErr.Load().(error); e != errNil {
			return 0, "", e
		}
		return 99, "bot", nil
	}
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	dockerOut := []byte("27.3.1\n")
	var dockerErr error
	var argv []string
	dockerRun := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		dockerCalls.Add(1)
		argv = append([]string{name}, args...)
		return dockerOut, dockerErr
	}
	rd := m.NewReadiness(ReadinessOptions{
		Checks:  []Check{GitLabCheck(gl), StoreCheck(st.Ping), DockerCheck("", dockerRun)},
		Secrets: []string{secret},
		Now:     clk.now,
	})

	code, rep := readyz(t, rd)
	if code != 200 || rep.Status != "ok" || len(rep.Checks) != 3 {
		t.Fatalf("ok case: %d %+v", code, rep)
	}
	for i, name := range []string{"gitlab", "store", "docker"} {
		if rep.Checks[i].Name != name || !rep.Checks[i].OK || rep.Checks[i].Error != "" {
			t.Fatalf("check %d: %+v", i, rep.Checks[i])
		}
	}
	if strings.Join(argv, " ") != "docker version --format {{.Server.Version}}" {
		t.Fatalf("docker argv: %v", argv)
	}

	// Cached for 30s: a GitLab outage is not seen yet, the store is
	// checked every time.
	glErr.Store(errors.New("gitlab: GET /user: HTTP 401: token " + secret + " revoked"))
	dockerErr, dockerOut = errors.New("exit status 1"), []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	if code, _ := readyz(t, rd); code != 200 || glCalls.Load() != 1 || dockerCalls.Load() != 1 {
		t.Fatalf("cache: %d gl=%d docker=%d", code, glCalls.Load(), dockerCalls.Load())
	}
	clk.add(31 * time.Second)
	code, rep = readyz(t, rd)
	if code != http.StatusServiceUnavailable || rep.Status != "fail" || glCalls.Load() != 2 || dockerCalls.Load() != 2 {
		t.Fatalf("failing: %d %+v", code, rep)
	}
	if rep.Checks[0].OK || strings.Contains(rep.Checks[0].Error, secret) || !strings.Contains(rep.Checks[0].Error, "HTTP 401") {
		t.Fatalf("gitlab error: %+v", rep.Checks[0])
	}
	if rep.Checks[2].OK || !strings.Contains(rep.Checks[2].Error, "Cannot connect to the Docker daemon") {
		t.Fatalf("docker error: %+v", rep.Checks[2])
	}
	if !rep.Checks[1].OK {
		t.Fatalf("store: %+v", rep.Checks[1])
	}

	// The gauge follows the same results.
	out := scrape(t, m)
	has(t, out, `reviewbot_readiness_check_ok{check="gitlab"} 0`, `reviewbot_readiness_check_ok{check="store"} 1`,
		`reviewbot_readiness_check_ok{check="docker"} 0`)

	// Store closed -> store fails without caching.
	st.Close()
	if _, rep = readyz(t, rd); rep.Checks[1].OK {
		t.Fatalf("closed store: %+v", rep.Checks[1])
	}
}

var errNil = errors.New("nil")

func TestDockerCheckTimeout(t *testing.T) {
	c := DockerCheck("docker", func(ctx context.Context, name string, args ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c.Timeout = 20 * time.Millisecond
	rd := (*Metrics)(nil).NewReadiness(ReadinessOptions{Checks: []Check{c}})
	rep := rd.Report(context.Background())
	if rep.Status != "fail" || rep.Checks[0].Error != "docker version: timeout" {
		t.Fatalf("%+v", rep)
	}
	// Empty server version (client present, daemon not answering).
	c2 := DockerCheck("docker", func(context.Context, string, ...string) ([]byte, error) { return []byte("\n"), nil })
	if rep := (*Metrics)(nil).NewReadiness(ReadinessOptions{Checks: []Check{c2}}).Report(context.Background()); rep.Status != "fail" {
		t.Fatalf("%+v", rep)
	}
}

// TestRealWebhookHandler checks the classification against the actual
// webhook handler's answers.
func TestRealWebhookHandler(t *testing.T) {
	m := New("")
	var jobs atomic.Int32
	h := m.WrapWebhook(webhook.NewHandler(webhook.Options{
		Secret: "hook-secret", Router: events.NewRouter(events.Options{BotUsername: "reviewbot"}),
		Enqueue: func(core.Job) { jobs.Add(1) },
	}))
	send := func(method, event, token, body string) int {
		r := httptest.NewRequest(method, "/webhook", bytes.NewBufferString(body))
		r.Header.Set("X-Gitlab-Event", event)
		r.Header.Set("X-Gitlab-Token", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	open := `{"object_kind":"merge_request","project":{"id":1,"path_with_namespace":"g/p"},` +
		`"object_attributes":{"iid":3,"state":"opened","action":"open","last_commit":{"id":"abc"}}}`
	draft := strings.Replace(open, `"state":"opened"`, `"state":"opened","draft":true`, 1)
	send(http.MethodGet, "Merge Request Hook", "hook-secret", "")
	send(http.MethodPost, "Note Hook", "bad", "{}")
	send(http.MethodPost, "Merge Request Hook", "hook-secret", "{not json")
	send(http.MethodPost, "Merge Request Hook", "hook-secret", open)
	send(http.MethodPost, "Merge Request Hook", "hook-secret", draft)
	send(http.MethodPost, "Push Hook", "hook-secret", "{}")
	if jobs.Load() != 1 {
		t.Fatalf("jobs %d", jobs.Load())
	}
	has(t, scrape(t, m),
		`reviewbot_webhook_events_total{event="merge_request",result="method_not_allowed"} 1`,
		`reviewbot_webhook_events_total{event="note",result="unauthorized"} 1`,
		`reviewbot_webhook_events_total{event="merge_request",result="bad_request"} 1`,
		`reviewbot_webhook_events_total{event="merge_request",result="accepted"} 1`,
		`reviewbot_webhook_events_total{event="merge_request",result="ignored"} 1`,
		`reviewbot_webhook_events_total{event="other",result="ignored"} 1`,
	)
}
