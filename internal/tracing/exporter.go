package tracing

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Exporter defaults.
const (
	DefaultQueueSize  = 2048
	DefaultMaxBatch   = 512
	DefaultInterval   = 5 * time.Second
	DefaultTimeout    = 10 * time.Second
	DefaultMaxRetries = 4
	defaultRetryBase  = 500 * time.Millisecond
	defaultRetryMax   = 30 * time.Second
	// TracesPath is appended to the endpoint (OTLP/HTTP).
	TracesPath = "/v1/traces"
	// maxErrorBody caps how much of an error response is read.
	maxErrorBody = 4 << 10
)

// Span export results (Observer, reviewbot_tracing_spans_total{result}).
const (
	ResultExported = "exported"
	ResultDropped  = "dropped" // queue full or exporter stopped: never sent
	ResultFailed   = "failed"  // sent but rejected, or retries exhausted
)

// Export error reasons (OnError, reviewbot_tracing_export_errors_total{reason}).
const (
	ErrReasonNetwork  = "network"   // no response: connection refused, DNS, TLS, timeout
	ErrReasonThrottle = "throttled" // 429
	ErrReasonServer   = "server"    // 5xx
	ErrReasonClient   = "client"    // other 4xx (bad auth, bad payload): not retried
	ErrReasonPartial  = "partial"   // 2xx with partialSuccess.rejectedSpans > 0
)

// ExporterOptions configure the OTLP/HTTP JSON exporter.
type ExporterOptions struct {
	// Endpoint is the collector base URL ("http://tempo:4318"); TracesPath
	// is appended unless the URL already ends with it.
	Endpoint string
	// Headers are sent with every request (e.g. Authorization). They are
	// never logged.
	Headers map[string]string
	// Timeout bounds one HTTP request (default DefaultTimeout).
	Timeout time.Duration
	// QueueSize caps spans waiting for export; more are dropped (default
	// DefaultQueueSize).
	QueueSize int
	// MaxBatch is the most spans per request (default DefaultMaxBatch).
	MaxBatch int
	// Interval is the flush period of a partial batch (default
	// DefaultInterval).
	Interval time.Duration
	// MaxRetries after the first attempt on 429/502/503/504 and network
	// errors (default DefaultMaxRetries; < 0 = none).
	MaxRetries int
	// RetryBase / RetryMax shape the exponential backoff (Retry-After wins
	// when the collector sends it, capped at RetryMax).
	RetryBase, RetryMax time.Duration
	// CAFile adds PEM certificates to the system roots (private CA).
	CAFile string
	// HTTPClient overrides the client (tests); CAFile and Timeout are then
	// not applied.
	HTTPClient *http.Client
	Logger     *slog.Logger
	// Observer counts spans by result (ResultExported, ResultDropped,
	// ResultFailed). It must be fast.
	Observer func(result string, n int)
	// OnError is told about every failed request attempt.
	OnError func(reason string)
}

// BatchExporter queues finished spans and sends them in batches from one
// goroutine. Enqueue never blocks: when the queue is full the span is
// dropped and counted. Shutdown flushes what is queued.
type BatchExporter struct {
	o      ExporterOptions
	url    string
	hc     *http.Client
	log    *slog.Logger
	ch     chan *SpanData
	flush  chan chan struct{}
	stop   chan struct{}
	done   chan struct{}
	ctx    context.Context // cancelled when Shutdown's deadline passes
	cancel context.CancelFunc

	stopOnce sync.Once
	stopped  atomic.Bool
	lastWarn atomic.Int64 // unix nanos of the last failure log line
}

var _ Exporter = (*BatchExporter)(nil)

// TracesURL returns the OTLP/HTTP traces URL for an endpoint.
func TracesURL(endpoint string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("tracing: endpoint must be an http(s) URL, got %q", RedactURL(endpoint))
	}
	u.RawQuery, u.Fragment = "", ""
	p := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(p, TracesPath) {
		p += TracesPath
	}
	u.Path, u.RawPath = p, ""
	return u.String(), nil
}

// RedactURL drops user info and the query from a URL for logs.
func RedactURL(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return "<invalid url>"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

// NewExporter validates options and starts the export goroutine.
func NewExporter(o ExporterOptions) (*BatchExporter, error) {
	u, err := TracesURL(o.Endpoint)
	if err != nil {
		return nil, err
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.QueueSize <= 0 {
		o.QueueSize = DefaultQueueSize
	}
	if o.MaxBatch <= 0 {
		o.MaxBatch = DefaultMaxBatch
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = DefaultMaxRetries
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.RetryBase <= 0 {
		o.RetryBase = defaultRetryBase
	}
	if o.RetryMax <= 0 {
		o.RetryMax = defaultRetryMax
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	hc := o.HTTPClient
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if o.CAFile != "" {
			pool, err := caPool(o.CAFile)
			if err != nil {
				return nil, err
			}
			tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
		}
		hc = &http.Client{Timeout: o.Timeout, Transport: tr}
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &BatchExporter{o: o, url: u, hc: hc, log: o.Logger, ch: make(chan *SpanData, o.QueueSize),
		flush: make(chan chan struct{}), stop: make(chan struct{}), done: make(chan struct{}), ctx: ctx, cancel: cancel}
	go e.loop()
	return e, nil
}

func caPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tracing: read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tracing: no certificates found in %s", path)
	}
	return pool, nil
}

// URL is the traces URL requests go to.
func (e *BatchExporter) URL() string { return e.url }

func (e *BatchExporter) observe(result string, n int) {
	if n > 0 && e.o.Observer != nil {
		e.o.Observer(result, n)
	}
}

func (e *BatchExporter) onError(reason string) {
	if e.o.OnError != nil {
		e.o.OnError(reason)
	}
}

// Enqueue queues a finished span; never blocks.
func (e *BatchExporter) Enqueue(s *SpanData) {
	if e == nil || s == nil {
		return
	}
	if e.stopped.Load() {
		e.observe(ResultDropped, 1)
		return
	}
	select {
	case e.ch <- s:
	default:
		e.observe(ResultDropped, 1)
	}
}

// ForceFlush sends everything queued so far and waits for it (or ctx).
func (e *BatchExporter) ForceFlush(ctx context.Context) error {
	ack := make(chan struct{})
	select {
	case e.flush <- ack:
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops accepting spans, sends what is queued and waits until
// done or ctx expires (pending retries are abandoned then, and their spans
// counted as failed). Safe to call more than once.
func (e *BatchExporter) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.stopOnce.Do(func() {
		e.stopped.Store(true)
		close(e.stop)
	})
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		e.cancel()
		<-e.done
		return ctx.Err()
	}
}

func (e *BatchExporter) loop() {
	defer close(e.done)
	defer e.cancel()
	t := time.NewTicker(e.o.Interval)
	defer t.Stop()
	var batch []*SpanData
	send := func() {
		if len(batch) > 0 {
			e.export(batch)
			batch = nil
		}
	}
	add := func(s *SpanData) {
		batch = append(batch, s)
		if len(batch) >= e.o.MaxBatch {
			send()
		}
	}
	drain := func() {
		for {
			select {
			case s := <-e.ch:
				add(s)
			default:
				send()
				return
			}
		}
	}
	for {
		select {
		case s := <-e.ch:
			add(s)
		case <-t.C:
			send()
		case ack := <-e.flush:
			drain()
			close(ack)
		case <-e.stop:
			drain()
			return
		}
	}
}

// export sends one batch with retries.
func (e *BatchExporter) export(batch []*SpanData) {
	body, err := encodeGzip(batch)
	if err != nil {
		e.warn("tracing: encode spans", "err", err)
		e.observe(ResultFailed, len(batch))
		return
	}
	for attempt := 0; ; attempt++ {
		if e.ctx.Err() != nil {
			e.observe(ResultFailed, len(batch))
			return
		}
		rejected, retryAfter, reason, err := e.post(body)
		if err == nil {
			e.observe(ResultExported, len(batch)-rejected)
			if rejected > 0 {
				e.onError(ErrReasonPartial)
				e.observe(ResultFailed, rejected)
			}
			return
		}
		e.onError(reason)
		retryable := reason == ErrReasonNetwork || reason == ErrReasonThrottle || (reason == ErrReasonServer && retryAfter >= 0)
		if !retryable || attempt >= e.o.MaxRetries {
			e.warn("tracing: export failed, spans dropped", "spans", len(batch), "attempts", attempt+1, "reason", reason, "err", err)
			e.observe(ResultFailed, len(batch))
			return
		}
		d := e.backoff(attempt, retryAfter)
		tm := time.NewTimer(d)
		select {
		case <-tm.C:
		case <-e.ctx.Done():
			tm.Stop()
		}
	}
}

func encodeGzip(batch []*SpanData) ([]byte, error) {
	raw, err := EncodeJSON(batch)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// post sends one request. A 5xx that OTLP says not to retry (500, 501,
// ...) returns retryAfter -1.
func (e *BatchExporter) post(body []byte) (rejected int, retryAfter time.Duration, reason string, err error) {
	ctx, cancel := context.WithTimeout(e.ctx, e.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return 0, 0, ErrReasonClient, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("User-Agent", "gitlab-review-bot-tracing/"+SDKVersion)
	for k, v := range e.o.Headers {
		req.Header.Set(k, v)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		// Never echo the request (headers); url.Error carries only the URL.
		return 0, 0, ErrReasonNetwork, errors.New(scrubURLError(err, e.url))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return partialRejected(data), 0, "", nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return 0, parseRetryAfter(resp.Header.Get("Retry-After")), ErrReasonThrottle, statusErr(resp.StatusCode, data)
	case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout:
		return 0, parseRetryAfter(resp.Header.Get("Retry-After")), ErrReasonServer, statusErr(resp.StatusCode, data)
	case resp.StatusCode >= 500:
		return 0, -1, ErrReasonServer, statusErr(resp.StatusCode, data)
	default:
		return 0, 0, ErrReasonClient, statusErr(resp.StatusCode, data)
	}
}

func statusErr(code int, body []byte) error {
	msg := strings.Join(strings.Fields(string(body)), " ")
	return fmt.Errorf("HTTP %d: %s", code, Truncate(msg, 200))
}

func scrubURLError(err error, u string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + " " + RedactURL(u) + ": " + ue.Err.Error()
	}
	return err.Error()
}

// partialRejected reads partialSuccess.rejectedSpans of an
// ExportTraceServiceResponse (int64: a JSON string or number).
func partialRejected(body []byte) int {
	var r struct {
		PartialSuccess struct {
			RejectedSpans json.RawMessage `json:"rejectedSpans"`
		} `json:"partialSuccess"`
	}
	if len(body) == 0 || json.Unmarshal(body, &r) != nil {
		return 0
	}
	s := strings.Trim(string(r.PartialSuccess.RejectedSpans), `"`)
	n, _ := strconv.Atoi(s)
	return max(n, 0)
}

func parseRetryAfter(v string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

func (e *BatchExporter) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, e.o.RetryMax)
	}
	d := e.o.RetryBase << attempt
	if d <= 0 || d > e.o.RetryMax {
		d = e.o.RetryMax
	}
	return d + time.Duration(rand.Int64N(int64(d)/5+1)) // up to 20% jitter
}

// warn logs at most once a minute: a collector that is down must not
// flood the bot's log.
func (e *BatchExporter) warn(msg string, args ...any) {
	now := time.Now().UnixNano()
	last := e.lastWarn.Load()
	if last != 0 && time.Duration(now-last) < time.Minute {
		return
	}
	if e.lastWarn.CompareAndSwap(last, now) {
		e.log.Warn(msg, append(args, "endpoint", RedactURL(e.url))...)
	}
}
