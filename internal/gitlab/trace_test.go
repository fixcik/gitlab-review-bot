package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

func TestURLTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"/api/v4/projects/42/merge_requests/7/notes":                                      "/projects/{id}/merge_requests/{id}/notes",
		"/gitlab/api/v4/projects/group%2Fapp/merge_requests":                              "/projects/{id}/merge_requests",
		"/api/v4/projects/42/repository/files/secret%2Fdir%2Fa.go/raw":                    "/projects/{id}/repository/files/{path}/raw",
		"/api/v4/projects/42/merge_requests/7/discussions/abc123def/notes":                "/projects/{id}/merge_requests/{id}/discussions/{id}/notes",
		"/api/v4/projects/42/repository/commits/0123456789abcdef0123456789abcdef01234567": "/projects/{id}/repository/commits/{sha}",
		"/api/v4/user": "/user",
	} {
		if got := URLTemplate(in); got != want {
			t.Errorf("URLTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

type sink struct {
	mu    sync.Mutex
	spans []*tracing.SpanData
}

func (s *sink) Enqueue(d *tracing.SpanData) { s.mu.Lock(); s.spans = append(s.spans, d); s.mu.Unlock() }

// TestClientSpans: inside a traced job every attempt is a client span with
// the templated path and the status; no token, query or file path.
func TestClientSpans(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte("content"))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "glpat-TOKENvalue123456", WithRetry(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	tr := tracing.New(tracing.Options{Exporter: s})
	ctx, root := tr.Start(context.Background(), "job")
	if _, err := c.GetRawFile(ctx, 42, "secret/dir/a.go", "feature/x"); err != nil {
		t.Fatal(err)
	}
	root.End()
	// Outside a trace: no span.
	if _, err := c.GetRawFile(context.Background(), 42, "a.go", "main"); err != nil {
		t.Fatal(err)
	}
	if len(s.spans) != 3 { // two attempts + the root
		t.Fatalf("spans = %d", len(s.spans))
	}
	first, second := s.spans[0], s.spans[1]
	if first.Name != "GET /projects/{id}/repository/files/{path}/raw" || first.Kind != tracing.KindClient {
		t.Fatalf("span %q kind %v", first.Name, first.Kind)
	}
	if v, _ := first.AttrValue("http.response.status_code"); v.Int != 502 || first.Status != tracing.StatusError {
		t.Fatalf("first attempt: %v %v", v, first.Status)
	}
	if v, _ := second.AttrValue("http.request.resend_count"); v.Int != 1 || second.Status == tracing.StatusError {
		t.Fatalf("retry: %v %v", v, second.Status)
	}
	if second.Parent != root.SpanContext().SpanID {
		t.Fatal("not a child of the job span")
	}
	for _, d := range s.spans {
		for _, a := range d.Attrs {
			v := a.Value.AsString()
			if strings.Contains(v, "glpat") || strings.Contains(v, "ref=") || strings.Contains(v, "secret") || strings.Contains(v, "feature") {
				t.Errorf("%s=%q leaks", a.Key, v)
			}
		}
	}
}

// A missing repository file is an answer, not a failure (optional files such
// as .reviewbot.yaml): its span is not an error. A 404 elsewhere, or any 5xx
// on a file, still is.
func TestClientSpansMissingFile(t *testing.T) {
	code := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	defer srv.Close()
	c, err := New(srv.URL, "glpat-TOKENvalue123456", WithRetry(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	tr := tracing.New(tracing.Options{Exporter: s})
	ctx, root := tr.Start(context.Background(), "job")
	_, _ = c.GetRawFile(ctx, 42, ".reviewbot.yaml", "main") // 404 on a file
	code = http.StatusInternalServerError
	_, _ = c.GetRawFile(ctx, 42, "a.go", "main") // 500 on a file
	code = http.StatusNotFound
	_, _ = c.GetMR(ctx, 42, 7) // 404 on an MR
	root.End()
	// Retries make several spans for the 500; look them up by what they are.
	var missing, serverErr, mr *tracing.SpanData
	for _, d := range s.spans {
		v, _ := d.AttrValue("http.response.status_code")
		switch {
		case v.Int == 404 && strings.Contains(d.Name, "repository/files"):
			missing = d
		case v.Int == 500:
			serverErr = d
		case v.Int == 404:
			mr = d
		}
	}
	if missing == nil || serverErr == nil || mr == nil {
		t.Fatalf("spans not found: %v %v %v", missing, serverErr, mr)
	}
	if v, _ := missing.AttrValue("http.response.status_code"); v.Int != 404 {
		t.Fatalf("status not recorded: %v", v)
	}
	if missing.Status == tracing.StatusError {
		t.Error("404 on a repository file is marked as an error")
	}
	if _, ok := missing.AttrValue("error.type"); ok {
		t.Error("404 on a repository file has error.type")
	}
	if serverErr.Status != tracing.StatusError {
		t.Error("500 on a repository file is not an error")
	}
	if mr.Status != tracing.StatusError {
		t.Error("404 on an MR is not an error")
	}
}

// A 404 whose body could not be read is a failed request, not "file missing".
func TestClientSpansMissingFileBodyReadFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("short"))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "glpat-TOKENvalue123456", WithRetry(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	tr := tracing.New(tracing.Options{Exporter: s})
	ctx, root := tr.Start(context.Background(), "job")
	_, _ = c.GetRawFile(ctx, 42, ".reviewbot.yaml", "main")
	root.End()
	req := s.spans[0]
	if v, _ := req.AttrValue("http.response.status_code"); v.Int != 404 {
		t.Fatalf("status: %v", v)
	}
	if req.Status != tracing.StatusError {
		t.Error("a 404 with a failed body read is not marked as an error")
	}
}
