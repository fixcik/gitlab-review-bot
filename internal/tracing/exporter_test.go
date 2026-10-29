package tracing

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// collector is a fake OTLP/HTTP endpoint that decodes gzip JSON requests.
type collector struct {
	t       *testing.T
	mu      sync.Mutex
	batches [][]string // span names per request
	headers []http.Header
	paths   []string
	reply   func(n int) (int, http.Header, string) // n = 1-based request number
	block   chan struct{}                          // when set, requests wait for it
	calls   atomic.Int32
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := int(c.calls.Add(1))
	if c.block != nil {
		<-c.block
	}
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Content-Encoding") != "gzip" {
		c.t.Errorf("bad request: %s %v", r.Method, r.Header)
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		c.t.Errorf("gzip: %v", err)
		return
	}
	raw, _ := io.ReadAll(zr)
	var req struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					Name string `json:"name"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		c.t.Errorf("json: %v", err)
	}
	var names []string
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				names = append(names, s.Name)
			}
		}
	}
	code, h, body := http.StatusOK, http.Header{}, "{}"
	if c.reply != nil {
		code, h, body = c.reply(n)
	}
	c.mu.Lock()
	if code >= 200 && code < 300 {
		c.batches = append(c.batches, names)
	}
	c.headers = append(c.headers, r.Header.Clone())
	c.paths = append(c.paths, r.URL.Path)
	c.mu.Unlock()
	for k, v := range h {
		w.Header()[k] = v
	}
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func (c *collector) received() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, b := range c.batches {
		n += len(b)
	}
	return n
}

type counts struct {
	mu     sync.Mutex
	result map[string]int
	errs   map[string]int
}

func newCounts() *counts { return &counts{result: map[string]int{}, errs: map[string]int{}} }

func (c *counts) observe(r string, n int) { c.mu.Lock(); c.result[r] += n; c.mu.Unlock() }
func (c *counts) onError(r string)        { c.mu.Lock(); c.errs[r]++; c.mu.Unlock() }
func (c *counts) get(r string) int        { c.mu.Lock(); defer c.mu.Unlock(); return c.result[r] }
func (c *counts) err(r string) int        { c.mu.Lock(); defer c.mu.Unlock(); return c.errs[r] }

func newTestExporter(t *testing.T, col *collector, o ExporterOptions) (*BatchExporter, *counts) {
	t.Helper()
	srv := httptest.NewServer(col)
	t.Cleanup(srv.Close)
	cnt := newCounts()
	o.Endpoint = srv.URL
	o.Observer, o.OnError = cnt.observe, cnt.onError
	if o.RetryBase == 0 {
		o.RetryBase = time.Millisecond
	}
	e, err := NewExporter(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Shutdown(context.Background()) })
	return e, cnt
}

func spanNamed(name string) *SpanData {
	now := time.Now()
	return &SpanData{Resource: Resource{ServiceName: "svc"}, SpanContext: SpanContext{TraceID: newTraceID(), SpanID: newSpanID(), Sampled: true},
		Name: name, Kind: KindInternal, Start: now, End: now}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTracesURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://tempo:4318":                       "http://tempo:4318/v1/traces",
		"http://tempo:4318/":                      "http://tempo:4318/v1/traces",
		"https://otlp.example.com/otlp":           "https://otlp.example.com/otlp/v1/traces",
		"https://otlp.example.com/otlp/v1/traces": "https://otlp.example.com/otlp/v1/traces",
	} {
		if got, err := TracesURL(in); err != nil || got != want {
			t.Errorf("TracesURL(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "tempo:4318", "ftp://x", "http://"} {
		if _, err := TracesURL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if got := RedactURL("https://user:pw@x.example/v1/traces?api_key=s"); got != "https://x.example/v1/traces" {
		t.Errorf("RedactURL = %q", got)
	}
}

func TestExporterBatchesAndHeaders(t *testing.T) {
	col := &collector{t: t}
	e, cnt := newTestExporter(t, col, ExporterOptions{MaxBatch: 2, Interval: time.Hour,
		Headers: map[string]string{"Authorization": "Bearer s3cret", "X-Scope-OrgID": "team"}})
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		e.Enqueue(spanNamed(n))
	}
	if err := e.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	col.mu.Lock()
	batches := col.batches
	col.mu.Unlock()
	if len(batches) != 3 || len(batches[0]) != 2 || len(batches[2]) != 1 {
		t.Fatalf("batches = %v", batches)
	}
	if col.paths[0] != TracesPath {
		t.Fatalf("path = %s", col.paths[0])
	}
	if col.headers[0].Get("Authorization") != "Bearer s3cret" || col.headers[0].Get("X-Scope-OrgID") != "team" {
		t.Fatalf("headers not sent: %v", col.headers[0])
	}
	if cnt.get(ResultExported) != 5 {
		t.Fatalf("exported = %d", cnt.get(ResultExported))
	}
}

func TestExporterFlushInterval(t *testing.T) {
	col := &collector{t: t}
	e, _ := newTestExporter(t, col, ExporterOptions{Interval: 20 * time.Millisecond})
	e.Enqueue(spanNamed("a"))
	waitFor(t, "interval flush", func() bool { return col.received() == 1 })
}

func TestExporterRetry(t *testing.T) {
	col := &collector{t: t, reply: func(n int) (int, http.Header, string) {
		switch n {
		case 1:
			return http.StatusServiceUnavailable, nil, "busy"
		case 2:
			return http.StatusTooManyRequests, http.Header{"Retry-After": {"0"}}, ""
		}
		return http.StatusOK, nil, `{"partialSuccess":{"rejectedSpans":"1","errorMessage":"too old"}}`
	}}
	e, cnt := newTestExporter(t, col, ExporterOptions{Interval: time.Hour})
	e.Enqueue(spanNamed("a"))
	e.Enqueue(spanNamed("b"))
	e.ForceFlush(context.Background())
	if col.calls.Load() != 3 {
		t.Fatalf("calls = %d", col.calls.Load())
	}
	if cnt.get(ResultExported) != 1 || cnt.get(ResultFailed) != 1 {
		t.Fatalf("results = %v", cnt.result)
	}
	if cnt.err(ErrReasonServer) != 1 || cnt.err(ErrReasonThrottle) != 1 || cnt.err(ErrReasonPartial) != 1 {
		t.Fatalf("errors = %v", cnt.errs)
	}
}

func TestExporterNoRetryOnClientError(t *testing.T) {
	col := &collector{t: t, reply: func(int) (int, http.Header, string) { return http.StatusUnauthorized, nil, "bad token" }}
	e, cnt := newTestExporter(t, col, ExporterOptions{Interval: time.Hour})
	e.Enqueue(spanNamed("a"))
	e.ForceFlush(context.Background())
	if col.calls.Load() != 1 || cnt.get(ResultFailed) != 1 || cnt.err(ErrReasonClient) != 1 {
		t.Fatalf("calls %d results %v errs %v", col.calls.Load(), cnt.result, cnt.errs)
	}
	// 500 is not retryable either (OTLP: only 429, 502, 503, 504).
	col2 := &collector{t: t, reply: func(int) (int, http.Header, string) { return http.StatusInternalServerError, nil, "" }}
	e2, cnt2 := newTestExporter(t, col2, ExporterOptions{Interval: time.Hour})
	e2.Enqueue(spanNamed("a"))
	e2.ForceFlush(context.Background())
	if col2.calls.Load() != 1 || cnt2.get(ResultFailed) != 1 {
		t.Fatalf("500 retried: %d", col2.calls.Load())
	}
}

func TestExporterRetriesExhaustedOnNetworkError(t *testing.T) {
	cnt := newCounts()
	e, err := NewExporter(ExporterOptions{Endpoint: "http://127.0.0.1:1", Interval: time.Hour, MaxRetries: 2, RetryBase: time.Millisecond,
		Timeout: 200 * time.Millisecond, Observer: cnt.observe, OnError: cnt.onError})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())
	e.Enqueue(spanNamed("a"))
	e.ForceFlush(context.Background())
	if cnt.err(ErrReasonNetwork) != 3 || cnt.get(ResultFailed) != 1 {
		t.Fatalf("errs %v results %v", cnt.errs, cnt.result)
	}
}

func TestExporterOverflowDropsWithoutBlocking(t *testing.T) {
	col := &collector{t: t, block: make(chan struct{})}
	e, cnt := newTestExporter(t, col, ExporterOptions{QueueSize: 3, MaxBatch: 1, Interval: time.Hour})
	e.Enqueue(spanNamed("first")) // taken by the loop, stuck in the request
	waitFor(t, "first request", func() bool { return col.calls.Load() == 1 })
	start := time.Now()
	for i := 0; i < 10; i++ {
		e.Enqueue(spanNamed("x"))
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("Enqueue blocked")
	}
	if cnt.get(ResultDropped) != 7 {
		t.Fatalf("dropped = %d, want 7 (queue of 3)", cnt.get(ResultDropped))
	}
	close(col.block)
	e.ForceFlush(context.Background())
	if cnt.get(ResultExported) != 4 {
		t.Fatalf("exported = %d", cnt.get(ResultExported))
	}
}

func TestExporterShutdownFlushes(t *testing.T) {
	col := &collector{t: t}
	e, cnt := newTestExporter(t, col, ExporterOptions{Interval: time.Hour})
	for i := 0; i < 3; i++ {
		e.Enqueue(spanNamed("x"))
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if col.received() != 3 {
		t.Fatalf("received %d", col.received())
	}
	e.Enqueue(spanNamed("late"))
	if cnt.get(ResultDropped) != 1 {
		t.Fatal("span after shutdown not counted as dropped")
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatal("second shutdown", err)
	}
}

func TestExporterShutdownDeadlineAbandonsRetries(t *testing.T) {
	col := &collector{t: t, reply: func(int) (int, http.Header, string) { return http.StatusServiceUnavailable, nil, "" }}
	e, cnt := newTestExporter(t, col, ExporterOptions{Interval: time.Hour, MaxRetries: 100, RetryBase: 50 * time.Millisecond})
	e.Enqueue(spanNamed("x"))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := e.Shutdown(ctx); err == nil {
		t.Fatal("expected deadline error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("shutdown hung")
	}
	if cnt.get(ResultFailed) != 1 {
		t.Fatalf("results %v", cnt.result)
	}
}

func TestTracerToExporterEndToEnd(t *testing.T) {
	col := &collector{t: t}
	e, _ := newTestExporter(t, col, ExporterOptions{Interval: time.Hour})
	tr := New(Options{Exporter: e, Resource: Resource{ServiceName: "svc"}})
	ctx, root := tr.Start(context.Background(), "root")
	_, c := Start(ctx, "child")
	c.End()
	root.End()
	e.Shutdown(context.Background())
	if col.received() != 2 {
		t.Fatalf("received %d", col.received())
	}
}
