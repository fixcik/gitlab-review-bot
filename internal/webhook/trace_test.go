package webhook

import (
	"net/http"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

type spanSink struct {
	mu    sync.Mutex
	spans []*tracing.SpanData
}

func (s *spanSink) Enqueue(d *tracing.SpanData) {
	s.mu.Lock()
	s.spans = append(s.spans, d)
	s.mu.Unlock()
}

// TestJobLinksToWebhookSpan: behind the tracing middleware an accepted
// job carries the request span's traceparent; without it, none.
func TestJobLinksToWebhookSpan(t *testing.T) {
	s := &spanSink{}
	tr := tracing.New(tracing.Options{Exporter: s})
	rec := &recorder{}
	h := tr.Handler("/webhook", newTestHandler(rec, ""), nil)
	if w := do(h, http.MethodPost, "Merge Request Hook", secret, mrHook("open", "opened", false, "", "")); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if len(rec.jobs) != 1 || len(s.spans) != 1 {
		t.Fatalf("jobs %d spans %d", len(rec.jobs), len(s.spans))
	}
	sc, err := tracing.ParseTraceParent(rec.jobs[0].TraceParent)
	if err != nil || sc.SpanID != s.spans[0].SpanContext.SpanID {
		t.Fatalf("traceparent %q", rec.jobs[0].TraceParent)
	}
	if v, _ := s.spans[0].AttrValue("reviewbot.webhook.result"); v.Str != "accepted" {
		t.Fatalf("result attr %v", v)
	}
	rec2 := &recorder{}
	do(newTestHandler(rec2, ""), http.MethodPost, "Merge Request Hook", secret, mrHook("open", "opened", false, "", ""))
	if rec2.jobs[0].TraceParent != "" {
		t.Fatal("traceparent without tracing")
	}
}
