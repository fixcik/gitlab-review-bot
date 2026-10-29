package tracing

import (
	"encoding/json"
	"os"
	"strconv"
	"time"
)

// OTLP/JSON encoding of ExportTraceServiceRequest
// (opentelemetry-proto, collector/trace/v1/trace_service.proto, with the
// OTLP/HTTP JSON mapping): lowerCamelCase field names, trace and span ids
// as lowercase hex strings (NOT base64: OTLP/JSON deviates from the
// standard protobuf JSON mapping here), 64-bit integers (timestamps,
// intValue) as decimal strings, enums as integers.

// ScopeName is the instrumentation scope of every span.
const ScopeName = "github.com/fixcik/gitlab-review-bot/internal/tracing"

// SDKVersion is reported as telemetry.sdk.version.
const SDKVersion = "1.0.0"

type otlpRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID                string         `json:"traceId"`
	SpanID                 string         `json:"spanId"`
	ParentSpanID           string         `json:"parentSpanId,omitempty"`
	Flags                  uint32         `json:"flags,omitempty"`
	Name                   string         `json:"name"`
	Kind                   int            `json:"kind"`
	StartTimeUnixNano      string         `json:"startTimeUnixNano"`
	EndTimeUnixNano        string         `json:"endTimeUnixNano"`
	Attributes             []otlpKeyValue `json:"attributes,omitempty"`
	DroppedAttributesCount int            `json:"droppedAttributesCount,omitempty"`
	Events                 []otlpEvent    `json:"events,omitempty"`
	DroppedEventsCount     int            `json:"droppedEventsCount,omitempty"`
	Links                  []otlpLink     `json:"links,omitempty"`
	Status                 *otlpStatus    `json:"status,omitempty"`
}

type otlpEvent struct {
	TimeUnixNano string         `json:"timeUnixNano"`
	Name         string         `json:"name"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpLink struct {
	TraceID    string         `json:"traceId"`
	SpanID     string         `json:"spanId"`
	Flags      uint32         `json:"flags,omitempty"`
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpStatus struct {
	Message string `json:"message,omitempty"`
	Code    int    `json:"code"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

// otlpAnyValue has exactly one field set.
type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // int64 as a decimal string
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

// W3C trace flags in the OTLP span/link flags field: bit 0 = sampled.
// (Bits 8-9, "has/is remote parent", are left unset: optional.)
const flagSampled = 0x01

func anyValue(v Value) otlpAnyValue {
	switch v.Type {
	case TypeInt:
		s := strconv.FormatInt(v.Int, 10)
		return otlpAnyValue{IntValue: &s}
	case TypeBool:
		b := v.Bool
		return otlpAnyValue{BoolValue: &b}
	case TypeFloat:
		f := v.F
		return otlpAnyValue{DoubleValue: &f}
	}
	s := v.Str
	return otlpAnyValue{StringValue: &s}
}

func keyValues(attrs []Attr) []otlpKeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]otlpKeyValue, len(attrs))
	for i, a := range attrs {
		out[i] = otlpKeyValue{Key: a.Key, Value: anyValue(a.Value)}
	}
	return out
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// ResourceAttrs are the OTLP resource attributes of r.
func ResourceAttrs(r Resource) []Attr {
	attrs := []Attr{
		String("service.name", r.ServiceName),
		String("telemetry.sdk.name", "reviewbot-tracing"),
		String("telemetry.sdk.language", "go"),
		String("telemetry.sdk.version", SDKVersion),
	}
	if r.ServiceVersion != "" {
		attrs = append(attrs, String("service.version", r.ServiceVersion))
	}
	if r.ServiceInstanceID != "" {
		attrs = append(attrs, String("service.instance.id", r.ServiceInstanceID))
	}
	if r.Environment != "" {
		// Both the classic and the current semantic-convention key.
		attrs = append(attrs, String("deployment.environment", r.Environment),
			String("deployment.environment.name", r.Environment))
	}
	return attrs
}

// DefaultInstanceID is the host name (the container id under docker), or
// a random id.
func DefaultInstanceID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return newSpanID().String()
}

// EncodeJSON encodes spans as one OTLP/JSON ExportTraceServiceRequest.
// Spans are grouped by resource (in practice there is one).
func EncodeJSON(spans []*SpanData) ([]byte, error) {
	req := otlpRequest{ResourceSpans: []otlpResourceSpans{}}
	idx := map[Resource]int{}
	for _, s := range spans {
		i, ok := idx[s.Resource]
		if !ok {
			i = len(req.ResourceSpans)
			idx[s.Resource] = i
			req.ResourceSpans = append(req.ResourceSpans, otlpResourceSpans{
				Resource:   otlpResource{Attributes: keyValues(ResourceAttrs(s.Resource))},
				ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: ScopeName, Version: SDKVersion}}},
			})
		}
		ss := &req.ResourceSpans[i].ScopeSpans[0]
		ss.Spans = append(ss.Spans, encodeSpan(s))
	}
	return json.Marshal(req)
}

func encodeSpan(s *SpanData) otlpSpan {
	o := otlpSpan{
		TraceID: s.SpanContext.TraceID.String(), SpanID: s.SpanContext.SpanID.String(),
		Name: s.Name, Kind: int(s.Kind),
		StartTimeUnixNano: nanos(s.Start), EndTimeUnixNano: nanos(s.End),
		Attributes: keyValues(s.Attrs), DroppedAttributesCount: s.DroppedAttrs, DroppedEventsCount: s.DroppedEvents,
	}
	if s.SpanContext.Sampled {
		o.Flags = flagSampled
	}
	if s.Parent.IsValid() {
		o.ParentSpanID = s.Parent.String()
	}
	for _, e := range s.Events {
		o.Events = append(o.Events, otlpEvent{TimeUnixNano: nanos(e.Time), Name: e.Name, Attributes: keyValues(e.Attrs)})
	}
	for _, l := range s.Links {
		ol := otlpLink{TraceID: l.SpanContext.TraceID.String(), SpanID: l.SpanContext.SpanID.String(), Attributes: keyValues(l.Attrs)}
		if l.SpanContext.Sampled {
			ol.Flags = flagSampled
		}
		o.Links = append(o.Links, ol)
	}
	if s.Status != StatusUnset {
		o.Status = &otlpStatus{Code: int(s.Status), Message: s.StatusMsg}
	}
	return o
}
