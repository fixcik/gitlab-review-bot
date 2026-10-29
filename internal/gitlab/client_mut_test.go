package gitlab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// redirectRT is a fake transport: every request is recorded; a request whose
// URL is in redirects gets a 302 to the mapped location, anything else 200.
type redirectRT struct {
	mu        sync.Mutex
	redirects map[string]string
	seen      []seenReq
}

type seenReq struct{ URL, Token, Auth string }

func (rt *redirectRT) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.seen = append(rt.seen, seenReq{r.URL.String(), r.Header.Get("PRIVATE-TOKEN"), r.Header.Get("Authorization")})
	loc, ok := rt.redirects[r.URL.String()]
	rt.mu.Unlock()
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: r}
	if ok {
		resp.StatusCode = http.StatusFound
		resp.Header.Set("Location", loc)
	}
	return resp, nil
}

func (rt *redirectRT) last() seenReq {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.seen[len(rt.seen)-1]
}

// The token stays only on the exact GitLab origin: a redirect to plain http
// on the same host, or to another port, drops it; a host differing only in
// case keeps it.
func TestRedirectTokenOrigin(t *testing.T) {
	const start = "https://gitlab.test/api/v4/projects/1/jobs/2/artifacts/a.xml"
	for _, tc := range []struct {
		to        string
		wantToken bool
	}{
		{"http://gitlab.test/api/v4/x", false},       // https -> http downgrade
		{"https://gitlab.test:8443/api/v4/x", false}, // other port
		{"https://gitlab.test.evil.com/x", false},
		{"https://GITLAB.test/api/v4/x", true},
		{"https://gitlab.test/api/v4/x", true},
	} {
		rt := &redirectRT{redirects: map[string]string{start: tc.to}}
		c, err := New("https://gitlab.test", testToken, WithHTTPClient(&http.Client{Transport: rt}), WithRetry(0, 0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetJobArtifact(context.Background(), 1, 2, "a.xml", 100); err != nil {
			t.Fatalf("%s: %v", tc.to, err)
		}
		got := rt.last()
		if got.URL != tc.to {
			t.Fatalf("%s: last request %s", tc.to, got.URL)
		}
		if (got.Token == testToken) != tc.wantToken || got.Auth != "" {
			t.Errorf("redirect to %s: token=%q auth=%q, want token kept=%v", tc.to, got.Token, got.Auth, tc.wantToken)
		}
	}
}

// A redirect loop stops after 10 redirects (Go's default limit, which a
// custom CheckRedirect must re-implement).
func TestRedirectLoopStops(t *testing.T) {
	var n atomic.Int32
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	})
	done := make(chan error, 1)
	go func() {
		_, err := c.GetJobArtifact(context.Background(), 1, 2, "a.xml", 100)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "redirects") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("redirect loop not stopped")
	}
	if got := n.Load(); got != 10 {
		t.Fatalf("requests = %d, want 10", got)
	}
}

// A body of exactly maxBytes is fine; one byte more is ErrTooLarge and is
// not retried (the same download would just be repeated).
func TestMaxBytesBoundaryAndNoRetry(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/1/jobs/2/artifacts/exact.xml":
			io.WriteString(w, strings.Repeat("x", 10))
		case "/api/v4/projects/1/jobs/2/artifacts/over.xml":
			io.WriteString(w, strings.Repeat("x", 11))
		}
	})
	b, err := c.GetJobArtifact(context.Background(), 1, 2, "exact.xml", 10)
	if err != nil || len(b) != 10 {
		t.Fatalf("exact size: %d bytes, %v", len(b), err)
	}
	n := len(f.requests())
	if _, err := c.GetJobArtifact(context.Background(), 1, 2, "over.xml", 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if got := len(f.requests()) - n; got != 1 {
		t.Fatalf("too-large response fetched %d times, want 1", got)
	}
	// The cap only applies to successful responses: a large error body is a
	// normal (truncated) *Error, not ErrTooLarge.
	f2, c2 := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, strings.Repeat("e", 50))
	})
	_ = f2
	if _, err := c2.GetJobArtifact(context.Background(), 1, 2, "a.xml", 10); errors.Is(err, ErrTooLarge) || StatusCode(err) != 404 {
		t.Fatalf("err = %v", err)
	}
}

func TestBackoff(t *testing.T) {
	c := &Client{baseDelay: 100 * time.Millisecond}
	for attempt, base := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond} {
		for i := 0; i < 20; i++ {
			d := c.backoff(attempt, 0)
			if d < base || d > base+base/5 {
				t.Fatalf("backoff(%d) = %v, want in [%v, %v]", attempt, d, base, base+base/5)
			}
		}
	}
	if d := c.backoff(0, 3*time.Second); d != 3*time.Second {
		t.Errorf("Retry-After not used: %v", d)
	}
	if d := c.backoff(0, 10*time.Minute); d != maxRetryDelay {
		t.Errorf("Retry-After not capped: %v", d)
	}
	if d := c.backoff(20, 0); d != maxRetryDelay {
		t.Errorf("exponential backoff not capped: %v", d)
	}
	if d := (&Client{}).backoff(0, 0); d != 0 {
		t.Errorf("zero base delay: %v", d)
	}
}
