package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

type sink struct {
	mu    sync.Mutex
	spans []*tracing.SpanData
}

func (s *sink) Enqueue(d *tracing.SpanData) { s.mu.Lock(); s.spans = append(s.spans, d); s.mu.Unlock() }

// TestCallSpans: in a traced job every tool call is a server span under the
// job span: tool name, status and sizes, never arguments or answers.
func TestCallSpans(t *testing.T) {
	s := &sink{}
	tr := tracing.New(tracing.Options{Exporter: s})
	jobCtx, job := tr.Start(context.Background(), "review.job")
	srv := New(Options{})
	ss, _, err := srv.Open(Spec{Ctx: jobCtx, TTL: time.Minute, Tools: []Tool{
		{Name: "read_file", Handler: func(context.Context, json.RawMessage) (string, error) { return "secret file content", nil }},
		{Name: "broken", Handler: func(context.Context, json.RawMessage) (string, error) { return "", errors.New("boom") }},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ss.Call(context.Background(), "read_file", json.RawMessage(`{"path":"secret/a.go"}`))
	ss.Call(context.Background(), "broken", nil)
	job.End()
	if len(s.spans) != 3 {
		t.Fatalf("spans %d", len(s.spans))
	}
	ok, bad := s.spans[0], s.spans[1]
	if ok.Name != "mcp tools/call read_file" || ok.Kind != tracing.KindServer || ok.Parent != job.SpanContext().SpanID {
		t.Fatalf("span %+v", ok)
	}
	if v, _ := ok.AttrValue("reviewbot.mcp.status"); v.Str != StatusOK {
		t.Fatalf("status %v", v)
	}
	if bad.Status != tracing.StatusError {
		t.Fatalf("error status %v", bad.Status)
	}
	for _, d := range s.spans {
		for _, a := range d.Attrs {
			if strings.Contains(a.Value.AsString(), "secret") {
				t.Errorf("%s leaks %q", a.Key, a.Value.AsString())
			}
		}
	}
}
