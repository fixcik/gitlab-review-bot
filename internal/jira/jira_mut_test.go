package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder is a scripted Jira: handler decides the answer, every request
// is recorded.
type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (r *recorder) server(t *testing.T, h func(w http.ResponseWriter, req *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.reqs = append(r.reqs, req)
		r.mu.Unlock()
		h(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *recorder) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, q := range r.reqs {
		p := q.URL.Path
		if q.URL.RawQuery != "" {
			p += "?" + q.URL.RawQuery
		}
		out = append(out, p)
	}
	return out
}

// An invalid key never reaches Jira (no path injection through the key).
func TestInvalidKeyNoRequest(t *testing.T) {
	var rec recorder
	srv := rec.server(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"key": "X", "fields": map[string]any{}})
	})
	c := newClient(t, srv.URL)
	for _, k := range []string{"../../admin", "PROJ-1/../../x", "proj-1", "PROJ-1?expand=all", "", "PROJ-"} {
		if _, err := c.GetIssue(context.Background(), k); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	if p := rec.paths(); len(p) != 0 {
		t.Fatalf("requests sent for invalid keys: %v", p)
	}
}

// The issue key is Jira's (a moved issue answers with its new key); without
// one the requested key is kept.
func TestIssueKeyFromResponse(t *testing.T) {
	var rec recorder
	srv := rec.server(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/OLD-1"):
			writeJSON(w, map[string]any{"key": "NEW-5", "fields": map[string]any{"summary": "moved"}})
		default:
			writeJSON(w, map[string]any{"fields": map[string]any{"summary": "no key"}})
		}
	})
	c := newClient(t, srv.URL, WithEpicLinkField(""), WithCommentsLimit(0))
	is, err := c.GetIssue(context.Background(), "OLD-1")
	if err != nil || is.Key != "NEW-5" || is.URL != srv.URL+"/ctx/browse/NEW-5" {
		t.Fatalf("moved issue: %+v %v", is, err)
	}
	is, err = c.GetIssue(context.Background(), "PROJ-3")
	if err != nil || is.Key != "PROJ-3" || is.URL != srv.URL+"/ctx/browse/PROJ-3" {
		t.Fatalf("no key in answer: %+v %v", is, err)
	}
}

// Parent and epic values that are not issue keys are ignored without a
// request; the epic brief asks only for summary and description.
func TestParentEpicKeysValidated(t *testing.T) {
	var rec recorder
	srv := rec.server(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/PROJ-1"):
			writeJSON(w, map[string]any{"key": "PROJ-1", "fields": map[string]any{
				"summary": "s", "parent": map[string]any{"key": "../admin"}, "customfield_10008": "not a key",
			}})
		case strings.HasSuffix(req.URL.Path, "/PROJ-2"):
			writeJSON(w, map[string]any{"key": "PROJ-2", "fields": map[string]any{"summary": "s", "customfield_10008": "PROJ-9"}})
		default:
			writeJSON(w, map[string]any{"key": "PROJ-9", "fields": map[string]any{"summary": "epic"}})
		}
	})
	c := newClient(t, srv.URL, WithCommentsLimit(0))
	is, err := c.GetIssue(context.Background(), "PROJ-1")
	if err != nil || is.Parent != nil || is.EpicKey != "" || is.Epic != nil {
		t.Fatalf("invalid parent/epic used: %+v %v", is, err)
	}
	if p := rec.paths(); len(p) != 1 {
		t.Fatalf("requests for invalid keys: %v", p)
	}
	is, err = c.GetIssue(context.Background(), "PROJ-2")
	if err != nil || is.Epic == nil || is.Epic.Summary != "epic" {
		t.Fatalf("epic: %+v %v", is, err)
	}
	p := rec.paths()
	if last := p[len(p)-1]; !strings.HasSuffix(last, "/PROJ-9?fields=summary%2Cdescription") {
		t.Errorf("epic brief request %q", last)
	}
}

func commentsField(total, startAt int, bodies ...string) map[string]any {
	var cs []any
	for _, b := range bodies {
		cs = append(cs, comment("a", b))
	}
	return map[string]any{"comments": cs, "total": total, "startAt": startAt, "maxResults": len(bodies)}
}

func commentBodies(is *Issue) string {
	var b []string
	for _, c := range is.Comments {
		b = append(b, c.Body)
	}
	return strings.Join(b, ",")
}

func TestCommentsPaging(t *testing.T) {
	var tailStatus atomic.Int32
	tailStatus.Store(200)
	var rec recorder
	var field map[string]any
	var tailQuery atomic.Value
	srv := rec.server(t, func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/comment") {
			tailQuery.Store(req.URL.RawQuery)
			if s := int(tailStatus.Load()); s != 200 {
				w.WriteHeader(s)
				return
			}
			writeJSON(w, map[string]any{"comments": []any{comment("b", "t1"), comment("b", "t2"), comment("b", "t3")}, "total": 3})
			return
		}
		writeJSON(w, map[string]any{"key": "PROJ-1", "fields": map[string]any{"summary": "s", "comment": field}})
	})
	ctx := context.Background()

	// Everything in the field, fewer than the limit: no extra request.
	field = commentsField(1, 0, "only")
	c := newClient(t, srv.URL, WithEpicLinkField(""), WithCommentsLimit(10))
	is, err := c.GetIssue(ctx, "PROJ-1")
	if err != nil || commentBodies(is) != "only" {
		t.Fatalf("single comment: %q %v", commentBodies(is), err)
	}
	if p := rec.paths(); len(p) != 1 {
		t.Fatalf("tail fetched although the field had every comment: %v", p)
	}

	// Partial field, total below the limit: the tail starts at 0.
	field = commentsField(3, 0, "f1")
	if is, err = c.GetIssue(ctx, "PROJ-1"); err != nil || commentBodies(is) != "t1,t2,t3" {
		t.Fatalf("tail: %q %v", commentBodies(is), err)
	}
	if q, _ := tailQuery.Load().(string); q != "maxResults=10&startAt=0" {
		t.Errorf("tail query %q", q)
	}

	// The tail request fails: the comments of the field are kept.
	tailStatus.Store(http.StatusBadRequest)
	field = commentsField(5, 0, "f1", "f2", "f3")
	c2 := newClient(t, srv.URL, WithEpicLinkField(""), WithCommentsLimit(2))
	if is, err = c2.GetIssue(ctx, "PROJ-1"); err != nil || commentBodies(is) != "f2,f3" {
		t.Fatalf("tail failure: %q %v", commentBodies(is), err)
	}
}

func TestRetryPolicy(t *testing.T) {
	// 500 is retried like the other 5xx.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"key": "PROJ-1", "fields": map[string]any{"summary": "ok"}})
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, WithEpicLinkField(""), WithCommentsLimit(0))
	if is, err := c.GetIssue(context.Background(), "PROJ-1"); err != nil || is.Summary != "ok" || calls.Load() != 2 {
		t.Fatalf("500 retry: %+v %v calls=%d", is, err, calls.Load())
	}

	// A long error body is cut, redacted and never retried for 400.
	calls.Store(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "%s %s", testToken, strings.Repeat("x", 5000))
	}))
	defer srv2.Close()
	c = newClient(t, srv2.URL)
	_, err := c.GetIssue(context.Background(), "PROJ-1")
	var je *Error
	if !errors.As(err, &je) || je.StatusCode != 400 || calls.Load() != 1 {
		t.Fatalf("400: %v calls=%d", err, calls.Load())
	}
	if strings.Contains(err.Error(), testToken) || len(je.Body) > maxErrorBody+len("...(truncated)") || !strings.HasSuffix(je.Body, "...(truncated)") {
		t.Errorf("error body %d bytes: %.80q", len(je.Body), je.Body)
	}
}

type failingTransport struct {
	calls atomic.Int32
	msg   string
}

func (f *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls.Add(1)
	return nil, errors.New(f.msg)
}

// Transport errors are retried and never carry the token.
func TestTransportErrorRetriedAndScrubbed(t *testing.T) {
	tr := &failingTransport{msg: "proxy said: bad credentials " + testToken}
	c, err := New("https://jira.example.com", testToken, WithHTTPClient(&http.Client{Transport: tr}), WithRetry(2, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetIssue(context.Background(), "PROJ-1")
	if err == nil || strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("err = %v", err)
	}
	if tr.calls.Load() != 3 {
		t.Errorf("transport error attempts = %d, want 3", tr.calls.Load())
	}
}

func TestParseRetryAfterAndBackoff(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 0, "5": 5 * time.Second, "-3": 0, "soon": 0,
		time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat): 0} {
		if got := parseRetryAfter(v); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", v, got, want)
		}
	}
	if d := parseRetryAfter(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)); d < 59*time.Minute || d > time.Hour {
		t.Errorf("future date: %v", d)
	}
	c := &Client{baseDelay: 100 * time.Millisecond}
	if d := c.backoff(0, 2*time.Minute); d != maxRetryDelay {
		t.Errorf("Retry-After is capped: %v", d)
	}
	if d := c.backoff(0, 3*time.Second); d != 3*time.Second {
		t.Errorf("Retry-After honoured: %v", d)
	}
	if d := c.backoff(2, 0); d < 400*time.Millisecond || d > 480*time.Millisecond {
		t.Errorf("exponential backoff with <=20%% jitter: %v", d)
	}
	if d := c.backoff(20, 0); d != maxRetryDelay {
		t.Errorf("backoff capped: %v", d)
	}
}
