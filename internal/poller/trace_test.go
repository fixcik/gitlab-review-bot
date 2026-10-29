package poller

import (
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

type sink struct {
	mu    sync.Mutex
	spans []*tracing.SpanData
}

func (s *sink) Enqueue(d *tracing.SpanData) { s.mu.Lock(); s.spans = append(s.spans, d); s.mu.Unlock() }

// TestTickTrace: every poll round is its own short trace and the jobs it
// enqueues carry its traceparent (the job trace links to it).
func TestTickTrace(t *testing.T) {
	h := newHarness(t)
	s := &sink{}
	h.p.o.Tracer = tracing.New(tracing.Options{Exporter: s})
	h.mr(1)
	h.tick() // baseline
	h.touch(2, func(m *gitlab.MRSummary) { m.SHA = "n1" })
	js := h.tick()
	wantKinds(t, js, core.JobAutoReview)
	if len(s.spans) != 2 || s.spans[1].Name != "poller.tick" || s.spans[1].Parent.IsValid() {
		t.Fatalf("spans: %+v", s.spans)
	}
	sc, err := tracing.ParseTraceParent(js[0].TraceParent)
	if err != nil || sc.SpanID != s.spans[1].SpanContext.SpanID {
		t.Fatalf("job traceparent %q does not point at the tick span", js[0].TraceParent)
	}
	if v, _ := s.spans[1].AttrValue("reviewbot.poll.jobs"); v.Int != 1 {
		t.Fatalf("jobs attr %v", v)
	}
	// Without a tracer: no link.
	h2 := newHarness(t)
	h2.mr(1)
	h2.tick()
	h2.touch(2, func(m *gitlab.MRSummary) { m.SHA = "n1" })
	if js := h2.tick(); js[0].TraceParent != "" {
		t.Fatalf("traceparent without tracing: %q", js[0].TraceParent)
	}
}
