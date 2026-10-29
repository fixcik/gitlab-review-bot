package gitlab

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Bad options and URLs are refused; a negative retry count means no retry.
func TestNewOptionErrors(t *testing.T) {
	if _, err := New("https://gitlab.example.com", testToken, WithHTTPClient(nil)); err == nil {
		t.Error("nil http client accepted")
	}
	if _, err := New("https://gitlab.example.com/%zz", testToken); err == nil || !strings.Contains(err.Error(), "invalid base URL") {
		t.Errorf("bad URL: %v", err)
	}
	var calls atomic.Int32
	hc := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("down")), Request: r}, nil
	})}
	c, err := New("https://gitlab.example.com", testToken, WithHTTPClient(hc), WithRetry(-1, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.CurrentUser(context.Background()); err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

// WithCAFile: the PEM must contain certificates; it is applied to a clone
// of the caller's *http.Transport (the caller's client and transport are
// not changed) and refused for any other RoundTripper.
func TestWithCAFileTransports(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": 1, "username": "tls"})
	}))
	defer srv.Close()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	junk := filepath.Join(dir, "junk.pem")
	os.WriteFile(junk, []byte("not a certificate"), 0o600)

	if _, err := New(srv.URL, testToken, WithCAFile(junk)); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Errorf("junk PEM: %v", err)
	}
	other := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("x") })}
	if _, err := New(srv.URL, testToken, WithHTTPClient(other), WithCAFile(caPath)); err == nil || !strings.Contains(err.Error(), "requires an *http.Transport") {
		t.Errorf("custom RoundTripper: %v", err)
	}

	for _, cfg := range []*tls.Config{nil, {MinVersion: tls.VersionTLS13}} {
		// HTTP/2 off: Clone then leaves a nil TLSClientConfig nil.
		tr := &http.Transport{TLSClientConfig: cfg, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
		hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}
		c, err := New(srv.URL, testToken, WithHTTPClient(hc), WithCAFile(caPath), WithRetry(0, 0))
		if err != nil {
			t.Fatal(err)
		}
		if _, name, err := c.CurrentUser(context.Background()); err != nil || name != "tls" {
			t.Fatalf("cfg %v: %q %v", cfg, name, err)
		}
		// (Transport.Clone may fill in the original's HTTP/2 defaults, never
		// its roots.)
		if hc.Transport != tr || hc.CheckRedirect != nil || (tr.TLSClientConfig != nil && tr.TLSClientConfig.RootCAs != nil) {
			t.Fatal("caller's client or transport modified")
		}
		got := c.hc.Transport.(*http.Transport).TLSClientConfig
		want := uint16(tls.VersionTLS12)
		if cfg != nil {
			want = cfg.MinVersion
		}
		if got.RootCAs == nil || got.MinVersion != want {
			t.Fatalf("tls config = %+v", got)
		}
	}
}

// A segment that is not valid percent-encoding is kept as given.
func TestEndpointUndecodableSegment(t *testing.T) {
	c, err := New("https://gitlab.example.com", testToken)
	if err != nil {
		t.Fatal(err)
	}
	u := c.endpoint(nil, "projects", "%zz")
	if u.Path != "/api/v4/projects/%zz" || u.RawQuery != "" {
		t.Fatalf("u = %#v", u)
	}
}

// Nothing is sent when the request cannot be encoded or built, and a
// cancelled context is reported as such (not as a transport error).
func TestDoEarlyFailures(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) })
	notes := c.endpoint(nil, "projects", "1", "merge_requests", "2", "notes")
	if _, err := c.do(context.Background(), request{method: http.MethodPost, u: notes, body: map[string]any{"bad": make(chan int)}}); err == nil || !strings.Contains(err.Error(), "encode request") {
		t.Errorf("encode: %v", err)
	}
	if _, err := c.once(context.Background(), request{method: "BAD METHOD", u: notes}, nil, 0); err == nil || !strings.Contains(err.Error(), "build request") {
		t.Errorf("build: %v", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.CurrentUser(ctx); err != context.Canceled {
		t.Errorf("cancelled: %v", err)
	}
}

type failingBody struct{ data string }

func (b *failingBody) Read(p []byte) (int, error) {
	if b.data == "" {
		return 0, errors.New("connection reset")
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

// A body that breaks while being read is an error for full reads, tail
// reads (job logs) and error responses alike.
func TestBodyReadErrors(t *testing.T) {
	status := 200
	hc := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(&failingBody{data: "partial"}), Request: r}, nil
	})}
	c, err := New("https://gitlab.example.com", testToken, WithHTTPClient(hc), WithRetry(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	u := c.endpoint(nil, "x")
	for _, tc := range []struct {
		status int
		tail   int64
	}{{200, 0}, {200, 4}, {500, 0}, {500, 4}} {
		status = tc.status
		_, err := c.do(context.Background(), request{method: http.MethodGet, u: u, tail: tc.tail})
		if err == nil || !strings.Contains(err.Error(), "read body") || !strings.Contains(err.Error(), "connection reset") {
			t.Errorf("status %d tail %d: %v", tc.status, tc.tail, err)
		}
	}
}

// Malformed JSON is an error for single objects and list pages.
func TestDecodeErrors(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"id":`) })
	var out struct{ ID int }
	if err := c.getJSON(context.Background(), c.endpoint(nil, "user"), &out); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Errorf("getJSON: %v", err)
	}
	if _, err := list[struct{ ID int }](context.Background(), c, nil, "projects"); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Errorf("list: %v", err)
	}
}

// A server that always announces another page stops the listing at
// maxPages with an error instead of looping forever.
func TestListStopsAtMaxPages(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("X-Next-Page", strconv.Itoa(p+1))
		io.WriteString(w, `[{"id":1}]`)
	})
	_, err := list[struct{ ID int }](context.Background(), c, nil, "projects", "1", "jobs")
	if err == nil || err.Error() != "gitlab: too many pages for projects/1/jobs" {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.requests()); n != maxPages {
		t.Fatalf("%d requests, want %d", n, maxPages)
	}
}

// Two clients sharing one http.Client keep their own redirect policy: a
// redirect from GitLab B to host A must not carry B's token (A's policy
// would allow its own host).
func TestSharedHTTPClientRedirectPolicy(t *testing.T) {
	const start = "https://b.test/api/v4/projects/1/jobs/2/artifacts/a.xml"
	rt := &redirectRT{redirects: map[string]string{start: "https://a.test/api/v4/x"}}
	hc := &http.Client{Transport: rt}
	if _, err := New("https://a.test", "glpat-token-of-a", WithHTTPClient(hc)); err != nil {
		t.Fatal(err)
	}
	b, err := New("https://b.test", testToken, WithHTTPClient(hc), WithRetry(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetJobArtifact(context.Background(), 1, 2, "a.xml", 100); err != nil {
		t.Fatal(err)
	}
	if got := rt.last(); got.URL != "https://a.test/api/v4/x" || got.Token != "" {
		t.Fatalf("redirected request %s carried token %q", got.URL, got.Token)
	}
	if hc.CheckRedirect != nil {
		t.Fatal("caller's client modified")
	}
}

// Retries are logged on the configured logger (never with the token); a
// nil logger keeps the default. StatusCode of a non-GitLab error is 0.
func TestRetryLoggingAndStatusCode(t *testing.T) {
	var logs bytes.Buffer
	n := 0
	f, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if n++; n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"id": 1, "username": "u"})
	})
	c, err := New(f.srv.URL, testToken, WithLogger(nil), WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))), WithRetry(1, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "gitlab: retrying request") || strings.Contains(logs.String(), testToken) {
		t.Fatalf("log: %s", logs.String())
	}
	if StatusCode(errors.New("x")) != 0 || StatusCode(&Error{StatusCode: 418}) != 418 {
		t.Fatal("StatusCode")
	}
}

// Every request names the bot in User-Agent; a 3xx that is not followed is
// an error, not a success.
func TestUserAgentAndNon2xx(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "gitlab-review-bot" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusMultipleChoices) // no Location: not followed
		io.WriteString(w, `{"id":1}`)
	})
	var out struct{ ID int }
	err := c.getJSON(context.Background(), c.endpoint(nil, "user"), &out)
	if StatusCode(err) != http.StatusMultipleChoices || out.ID != 0 {
		t.Fatalf("err = %v, out = %+v", err, out)
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("%d requests", n)
	}
}

// Retry-After is read from 429 and every 5xx (500 included), not from
// other errors.
func TestRetryAfterParsedFor5xx(t *testing.T) {
	status := 500
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
	})
	for code, want := range map[int]time.Duration{500: 7 * time.Second, 503: 7 * time.Second, 429: 7 * time.Second, 404: 0, 499: 0} {
		status = code
		_, err := c.once(context.Background(), request{method: http.MethodGet, u: c.endpoint(nil, "user")}, nil, 0)
		var ge *Error
		if !errors.As(err, &ge) || ge.StatusCode != code || ge.retryAfter != want {
			t.Errorf("%d: %v retryAfter=%v, want %v", code, err, ge.retryAfter, want)
		}
	}
}

// List pages are GETs: a transient 5xx is retried.
func TestListRetriesServerErrors(t *testing.T) {
	n := 0
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if n++; n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `[{"id":1},{"id":2}]`)
	})
	got, err := list[struct{ ID int }](context.Background(), c, nil, "projects")
	if err != nil || len(got) != 2 || n != 2 {
		t.Fatalf("got %v, %v after %d requests", got, err, n)
	}
}

type countingBody struct {
	n    int64
	left int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		return 0, io.EOF
	}
	k := min(int64(len(p)), b.left)
	b.n += k
	b.left -= k
	return int(k), nil
}

// A capped request reads at most maxBytes+1 bytes of an oversized body.
func TestMaxBytesBoundsTheRead(t *testing.T) {
	body := &countingBody{left: 50 << 20}
	hc := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(body), Request: r}, nil
	})}
	c, err := New("https://gitlab.example.com", testToken, WithHTTPClient(hc), WithRetry(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.do(context.Background(), request{method: http.MethodGet, u: c.endpoint(nil, "x"), maxBytes: 10})
	if !errors.Is(err, ErrTooLarge) || body.n > 1<<16 {
		t.Fatalf("err=%v read=%d", err, body.n)
	}
}
