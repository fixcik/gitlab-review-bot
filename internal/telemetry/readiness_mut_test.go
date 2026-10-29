package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A check cut short because the caller gave up (probe client disconnected,
// the scrape's shorter bound) is not cached: the next caller runs it again.
// A failure of the check itself is cached as usual.
func TestReadinessCallerCancellationNotCached(t *testing.T) {
	var calls atomic.Int32
	var slow, fail atomic.Bool
	c := Check{Name: "gitlab", CacheFor: 30 * time.Second, Timeout: 10 * time.Second, Run: func(ctx context.Context) error {
		calls.Add(1)
		if slow.Load() {
			<-ctx.Done()
			return ctx.Err()
		}
		if fail.Load() {
			return errors.New("HTTP 502")
		}
		return nil
	}}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	rd := (*Metrics)(nil).NewReadiness(ReadinessOptions{Checks: []Check{c}, Now: clk.now})

	slow.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	rep := rd.Report(ctx)
	cancel()
	if rep.Status != "fail" {
		t.Fatalf("cancelled caller: %+v", rep)
	}
	slow.Store(false)
	if rep := rd.Report(context.Background()); rep.Status != "ok" || calls.Load() != 2 {
		t.Fatalf("cancelled result served from cache: %+v calls=%d", rep, calls.Load())
	}

	// A real failure is cached for CacheFor.
	clk.add(31 * time.Second)
	fail.Store(true)
	if rep := rd.Report(context.Background()); rep.Status != "fail" || calls.Load() != 3 {
		t.Fatalf("failure: %+v calls=%d", rep, calls.Load())
	}
	fail.Store(false)
	clk.add(10 * time.Second)
	if rep := rd.Report(context.Background()); rep.Status != "fail" || calls.Load() != 3 {
		t.Fatalf("failure must stay cached: %+v calls=%d", rep, calls.Load())
	}
}

// Secrets are removed whole even when one contains another; values too
// short to be secrets do not mangle the text; long errors are cut.
func TestReadinessScrub(t *testing.T) {
	msg := "auth abcd1234-and-more failed for ab"
	rd := (*Metrics)(nil).NewReadiness(ReadinessOptions{
		Secrets: []string{"abcd1234", " abcd1234-and-more ", "ab"},
		Checks:  []Check{{Name: "x", Run: func(context.Context) error { return errors.New(msg) }}},
	})
	rep := rd.Report(context.Background())
	if got := rep.Checks[0].Error; got != "auth [скрыто] failed for ab" {
		t.Fatalf("scrubbed error %q", got)
	}

	long := strings.Repeat("e", 1000)
	rd = (*Metrics)(nil).NewReadiness(ReadinessOptions{
		Checks: []Check{{Name: "x", Run: func(context.Context) error { return errors.New(long) }}},
	})
	if got := rd.Report(context.Background()).Checks[0].Error; got != strings.Repeat("e", 300)+"…" {
		t.Fatalf("long error not cut: %d bytes", len(got))
	}
}

// Timeout 0 means the default bound, not an immediate deadline.
func TestReadinessDefaultTimeout(t *testing.T) {
	c := Check{Name: "x", Run: func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
			return nil
		}
	}}
	rd := (*Metrics)(nil).NewReadiness(ReadinessOptions{Checks: []Check{c}})
	if rep := rd.Report(context.Background()); rep.Status != "ok" {
		t.Fatalf("%+v", rep)
	}
	// A configured timeout is kept.
	hang := Check{Name: "y", Timeout: 30 * time.Millisecond, Run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	start := time.Now()
	rep := (*Metrics)(nil).NewReadiness(ReadinessOptions{Checks: []Check{hang}}).Report(context.Background())
	if rep.Status != "fail" || time.Since(start) > 2*time.Second {
		t.Fatalf("configured timeout ignored: %+v after %v", rep, time.Since(start))
	}
}

func TestReadyzResponse(t *testing.T) {
	var fail atomic.Bool
	rd := (*Metrics)(nil).NewReadiness(ReadinessOptions{Checks: []Check{{Name: "x", Run: func(context.Context) error {
		if fail.Load() {
			return errors.New("down")
		}
		return nil
	}}}})
	w := httptest.NewRecorder()
	rd.ServeHTTP(w, httptest.NewRequest(http.MethodGet, ReadyPath, nil))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("ok: %d %v", w.Code, w.Header())
	}
	fail.Store(true)
	w = httptest.NewRecorder()
	rd.ServeHTTP(w, httptest.NewRequest(http.MethodHead, ReadyPath, nil))
	if w.Code != http.StatusServiceUnavailable || w.Body.Len() != 0 {
		t.Fatalf("HEAD fail: %d %q", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	rd.ServeHTTP(w, httptest.NewRequest(http.MethodPost, ReadyPath, nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: %d", w.Code)
	}
}
