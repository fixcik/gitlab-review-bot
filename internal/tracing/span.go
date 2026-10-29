package tracing

import (
	"fmt"
	"sync"
	"time"
)

// Span is one timed operation. All methods are safe for concurrent use and
// no-ops on a nil span; a span that is not sampled keeps nothing.
type Span struct {
	tr     *Tracer
	sc     SpanContext
	parent SpanID
	kind   Kind
	start  time.Time

	mu            sync.Mutex
	name          string
	attrs         []Attr
	droppedAttrs  int
	events        []Event
	droppedEvents int
	links         []Link
	status        StatusCode
	statusMsg     string
	ended         bool
}

// SpanContext returns the span's ids (zero for a nil span).
func (s *Span) SpanContext() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	return s.sc
}

// TraceID returns the hex trace id ("" for a nil span).
func (s *Span) TraceID() string {
	if s == nil {
		return ""
	}
	return s.sc.TraceID.String()
}

// IsRecording reports whether the span keeps data (sampled, not ended).
func (s *Span) IsRecording() bool {
	if s == nil || !s.sc.Sampled {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.ended
}

// SetName renames the span.
func (s *Span) SetName(name string) {
	if s == nil || !s.sc.Sampled {
		return
	}
	s.mu.Lock()
	if !s.ended {
		s.name = name
	}
	s.mu.Unlock()
}

// SetAttr sets attributes (a key set again replaces the earlier value).
func (s *Span) SetAttr(attrs ...Attr) {
	if s == nil || !s.sc.Sampled || len(attrs) == 0 {
		return
	}
	attrs = s.clean(attrs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	for _, a := range attrs {
		s.setLocked(a)
	}
}

func (s *Span) setLocked(a Attr) {
	for i := range s.attrs {
		if s.attrs[i].Key == a.Key {
			s.attrs[i] = a
			return
		}
	}
	if len(s.attrs) >= MaxSpanAttrs {
		s.droppedAttrs++
		return
	}
	s.attrs = append(s.attrs, a)
}

// SetAttrIfAbsent sets each attribute whose key is not set yet.
func (s *Span) SetAttrIfAbsent(attrs ...Attr) {
	if s == nil || !s.sc.Sampled || len(attrs) == 0 {
		return
	}
	attrs = s.clean(attrs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	for _, a := range attrs {
		if _, set := findAttr(s.attrs, a.Key); !set {
			s.setLocked(a)
		}
	}
}

// Attr returns the current value of an attribute.
func (s *Span) Attr(key string) (Value, bool) {
	if s == nil {
		return Value{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return findAttr(s.attrs, key)
}

// AddEvent adds a timestamped event.
func (s *Span) AddEvent(name string, attrs ...Attr) {
	if s == nil || !s.sc.Sampled {
		return
	}
	attrs = s.clean(attrs)
	now := s.tr.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	if len(s.events) >= MaxSpanEvents {
		s.droppedEvents++
		return
	}
	s.events = append(s.events, Event{Name: name, Time: now, Attrs: attrs})
}

// RecordError adds an "exception" event (exception.type = class,
// exception.message = the scrubbed, capped error text) and sets the
// status to Error. class is a short error class ("timeout", "http_5xx");
// "" uses the Go type of err.
func (s *Span) RecordError(err error, class string) {
	if s == nil || err == nil || !s.sc.Sampled {
		return
	}
	if class == "" {
		class = fmt.Sprintf("%T", err)
	}
	msg := err.Error()
	s.AddEvent("exception", String("exception.type", class), String("exception.message", msg))
	s.SetAttr(String("error.type", class))
	s.SetStatus(StatusError, msg)
}

// SetStatus sets the status. An Error is not downgraded to OK; the message
// is kept only for Error (as OTLP asks).
func (s *Span) SetStatus(code StatusCode, msg string) {
	if s == nil || !s.sc.Sampled {
		return
	}
	if code != StatusError {
		msg = ""
	} else {
		msg = s.tr.cleanString(msg)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended || (s.status == StatusError && code != StatusError) {
		return
	}
	s.status, s.statusMsg = code, msg
}

// End finishes the span and hands it to the exporter (once).
func (s *Span) End() { s.EndAt(time.Time{}) }

// EndAt finishes the span at t (zero = now).
func (s *Span) EndAt(t time.Time) {
	if s == nil || !s.sc.Sampled {
		return
	}
	if t.IsZero() {
		t = s.tr.now()
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	if t.Before(s.start) {
		t = s.start
	}
	d := &SpanData{
		Resource: s.tr.res, SpanContext: s.sc, Parent: s.parent, Name: s.name, Kind: s.kind,
		Start: s.start, End: t, Attrs: s.attrs, DroppedAttrs: s.droppedAttrs, Events: s.events,
		DroppedEvents: s.droppedEvents, Links: s.links, Status: s.status, StatusMsg: s.statusMsg,
	}
	s.mu.Unlock()
	if s.tr.exp != nil {
		s.tr.exp.Enqueue(d)
	}
}

// SpanData is a finished span as the exporter sees it.
type SpanData struct {
	Resource      Resource
	SpanContext   SpanContext
	Parent        SpanID // zero for a root span
	Name          string
	Kind          Kind
	Start, End    time.Time
	Attrs         []Attr
	DroppedAttrs  int
	Events        []Event
	DroppedEvents int
	Links         []Link
	Status        StatusCode
	StatusMsg     string
}

// AttrValue returns the value of an attribute of a finished span.
func (d *SpanData) AttrValue(key string) (Value, bool) { return findAttr(d.Attrs, key) }
