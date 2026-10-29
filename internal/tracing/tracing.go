// Package tracing is a small, dependency-free OpenTelemetry-compatible
// tracer: spans with attributes, events, links and status, W3C trace
// context (traceparent), head sampling by ratio, and an OTLP/HTTP JSON
// exporter (exporter.go, otlp.go). It exists because the official Go SDK
// cannot be vendored in this project's build environment; the wire format
// is the standard OTLP one, so Tempo, the OpenTelemetry Collector, Grafana
// Alloy and Grafana Cloud accept it as is.
//
// The API is deliberately tiny:
//
//	ctx, sp := tracer.Start(ctx, "review.job", tracing.WithKind(tracing.KindConsumer))
//	defer sp.End()
//	ctx, child := tracing.Start(ctx, "git.diff") // child of the span in ctx
//	child.SetAttr(tracing.Int("reviewbot.diff_bytes", n))
//	child.End()
//
// A nil *Tracer and a nil *Span are valid and do nothing, and the
// package-level Start only creates a span when ctx already carries one: code
// outside a traced job (or with tracing disabled) pays one context lookup.
//
// Privacy: every string attribute, event attribute and status message goes
// through the tracer's scrubber (Options.Scrub) and is capped at
// MaxAttrLen bytes. Callers must still never pass prompt text, code, note
// bodies or logs: spans carry ids, sizes, counts, durations and statuses.
package tracing

import (
	"context"
	"encoding/binary"
	"math"
	"time"
)

// MaxAttrLen caps string attribute values and status messages (bytes).
const MaxAttrLen = 256

// MaxSpanAttrs, MaxSpanEvents and MaxSpanLinks cap what one span keeps;
// the rest is dropped (and counted in the OTLP dropped*Count fields).
const (
	MaxSpanAttrs  = 64
	MaxSpanEvents = 32
	MaxSpanLinks  = 8
)

// Resource describes the process (OTLP resource attributes).
type Resource struct {
	ServiceName       string
	ServiceVersion    string
	ServiceInstanceID string
	Environment       string // deployment.environment; "" = not set
}

// Exporter receives finished, sampled spans. Enqueue must never block.
type Exporter interface {
	Enqueue(s *SpanData)
}

// Options configure a Tracer.
type Options struct {
	Resource Resource
	// SampleRatio is the share of new traces kept, 0..1 (default 1; values
	// <= 0 keep nothing, >= 1 keep everything). Children follow their
	// parent's decision, so a trace is always complete or absent.
	SampleRatio *float64
	Exporter    Exporter
	// Scrub cleans every string value that goes into a span (secrets,
	// tokens, URLs with credentials). nil = no scrubbing.
	Scrub func(string) string
	Now   func() time.Time
}

// Tracer creates spans. A nil *Tracer creates none.
type Tracer struct {
	res       Resource
	threshold uint64 // keep a new trace when the low 8 bytes of its id < threshold (or all)
	all, none bool
	exp       Exporter
	scrub     func(string) string
	now       func() time.Time
}

// New creates a tracer.
func New(o Options) *Tracer {
	t := &Tracer{res: o.Resource, exp: o.Exporter, scrub: o.Scrub, now: o.Now}
	if t.now == nil {
		t.now = time.Now
	}
	ratio := 1.0
	if o.SampleRatio != nil {
		ratio = *o.SampleRatio
	}
	switch {
	case ratio >= 1 || math.IsNaN(ratio):
		t.all = true
	case ratio <= 0:
		t.none = true
	default:
		t.threshold = uint64(ratio * math.MaxUint64)
	}
	return t
}

// Resource returns the tracer's resource.
func (t *Tracer) Resource() Resource {
	if t == nil {
		return Resource{}
	}
	return t.res
}

// Enabled reports whether t creates spans.
func (t *Tracer) Enabled() bool { return t != nil }

// sample decides for a new trace (TraceIdRatioBased: the low 8 bytes of
// the random trace id against the ratio).
func (t *Tracer) sample(id TraceID) bool {
	switch {
	case t.all:
		return true
	case t.none:
		return false
	}
	return binary.BigEndian.Uint64(id[8:]) < t.threshold
}

// Start starts a span: a child of the span (or remote parent) in ctx, or
// the root of a new trace. It returns ctx carrying the span. A nil Tracer
// returns ctx and a nil span.
func (t *Tracer) Start(ctx context.Context, name string, opts ...StartOption) (context.Context, *Span) {
	if t == nil {
		return ctx, nil
	}
	var c startConfig
	for _, o := range opts {
		o(&c)
	}
	var parent SpanContext
	hasParent := false
	if !c.newRoot {
		parent, hasParent = SpanContextFromContext(ctx)
	}
	sp := &Span{tr: t, name: name, kind: c.kind}
	if sp.kind == KindUnspecified {
		sp.kind = KindInternal
	}
	if hasParent && parent.IsValid() {
		sp.sc = SpanContext{TraceID: parent.TraceID, Sampled: parent.Sampled}
		sp.parent = parent.SpanID
	} else {
		sp.sc.TraceID = newTraceID()
		sp.sc.Sampled = t.sample(sp.sc.TraceID)
	}
	sp.sc.SpanID = newSpanID()
	sp.start = c.start
	if sp.start.IsZero() {
		sp.start = t.now()
	}
	if sp.sc.Sampled {
		sp.SetAttr(c.attrs...)
		for _, l := range c.links {
			if len(sp.links) < MaxSpanLinks {
				sp.links = append(sp.links, Link{SpanContext: l.SpanContext, Attrs: sp.clean(l.Attrs)})
			}
		}
	}
	return ContextWithSpan(ctx, sp), sp
}

// Start starts a child of the span in ctx with that span's tracer. Without
// a span in ctx it does nothing and returns ctx and a nil span: spans are
// only created inside a traced unit of work (a job, a webhook request).
func Start(ctx context.Context, name string, opts ...StartOption) (context.Context, *Span) {
	parent := SpanFromContext(ctx)
	if parent == nil {
		return ctx, nil
	}
	return parent.tr.Start(ctx, name, opts...)
}

type startConfig struct {
	kind    Kind
	attrs   []Attr
	links   []Link
	start   time.Time
	newRoot bool
}

// StartOption configures Start.
type StartOption func(*startConfig)

// WithKind sets the span kind (default internal).
func WithKind(k Kind) StartOption { return func(c *startConfig) { c.kind = k } }

// WithAttrs sets attributes at start.
func WithAttrs(attrs ...Attr) StartOption {
	return func(c *startConfig) { c.attrs = append(c.attrs, attrs...) }
}

// WithLinks links the span to other spans.
func WithLinks(links ...Link) StartOption {
	return func(c *startConfig) {
		for _, l := range links {
			if l.SpanContext.IsValid() {
				c.links = append(c.links, l)
			}
		}
	}
}

// WithStartTime backdates the span (for work measured before the span
// was created, e.g. an MCP tool call reported after it finished).
func WithStartTime(t time.Time) StartOption { return func(c *startConfig) { c.start = t } }

// WithNewRoot ignores any parent in ctx: the span starts a new trace.
func WithNewRoot() StartOption { return func(c *startConfig) { c.newRoot = true } }
