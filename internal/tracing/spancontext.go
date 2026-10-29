package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// TraceID is a 16-byte trace id.
type TraceID [16]byte

// SpanID is an 8-byte span id.
type SpanID [8]byte

// IsValid reports whether the id is not all zeros.
func (t TraceID) IsValid() bool { return t != TraceID{} }

// IsValid reports whether the id is not all zeros.
func (s SpanID) IsValid() bool { return s != SpanID{} }

// String is the lowercase hex form (32 characters).
func (t TraceID) String() string { return hex.EncodeToString(t[:]) }

// String is the lowercase hex form (16 characters).
func (s SpanID) String() string { return hex.EncodeToString(s[:]) }

// SpanContext identifies a span across process boundaries.
type SpanContext struct {
	TraceID TraceID
	SpanID  SpanID
	Sampled bool
	// Remote: parsed from a traceparent (not created in this process).
	Remote bool
}

// IsValid reports whether both ids are set.
func (sc SpanContext) IsValid() bool { return sc.TraceID.IsValid() && sc.SpanID.IsValid() }

// TraceParent formats sc as a W3C traceparent header value
// ("00-<trace id>-<span id>-<flags>"); "" when sc is not valid.
func (sc SpanContext) TraceParent() string {
	if !sc.IsValid() {
		return ""
	}
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return "00-" + sc.TraceID.String() + "-" + sc.SpanID.String() + "-" + flags
}

// ErrBadTraceParent is returned by ParseTraceParent.
var ErrBadTraceParent = errors.New("tracing: invalid traceparent")

// ParseTraceParent parses a W3C traceparent header value. Version 00 must
// have exactly four fields; a higher version may carry more (they are
// ignored, as the spec asks). Version ff, all-zero ids and non-hex or
// upper-case characters are rejected.
func ParseTraceParent(s string) (SpanContext, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, "-")
	if len(parts) < 4 {
		return SpanContext{}, ErrBadTraceParent
	}
	ver, tid, sid, flags := parts[0], parts[1], parts[2], parts[3]
	if len(ver) != 2 || !lowerHex(ver) || ver == "ff" || (ver == "00" && len(parts) != 4) {
		return SpanContext{}, ErrBadTraceParent
	}
	if len(tid) != 32 || len(sid) != 16 || len(flags) != 2 || !lowerHex(tid) || !lowerHex(sid) || !lowerHex(flags) {
		return SpanContext{}, ErrBadTraceParent
	}
	var sc SpanContext
	hex.Decode(sc.TraceID[:], []byte(tid))
	hex.Decode(sc.SpanID[:], []byte(sid))
	var f [1]byte
	hex.Decode(f[:], []byte(flags))
	if !sc.IsValid() {
		return SpanContext{}, ErrBadTraceParent
	}
	sc.Sampled = f[0]&1 == 1
	sc.Remote = true
	return sc, nil
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func newTraceID() TraceID {
	var t TraceID
	for !t.IsValid() {
		_, _ = rand.Read(t[:])
	}
	return t
}

func newSpanID() SpanID {
	var s SpanID
	for !s.IsValid() {
		_, _ = rand.Read(s[:])
	}
	return s
}

type spanKey struct{}
type remoteKey struct{}

// ContextWithSpan returns ctx carrying sp (ctx unchanged for a nil span).
func ContextWithSpan(ctx context.Context, sp *Span) context.Context {
	if sp == nil {
		return ctx
	}
	return context.WithValue(ctx, spanKey{}, sp)
}

// SpanFromContext returns the span in ctx, or nil.
func SpanFromContext(ctx context.Context) *Span {
	if ctx == nil {
		return nil
	}
	sp, _ := ctx.Value(spanKey{}).(*Span)
	return sp
}

// ContextWithRemote returns ctx carrying a remote parent (an incoming
// traceparent): the next Tracer.Start becomes its child.
func ContextWithRemote(ctx context.Context, sc SpanContext) context.Context {
	if !sc.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, remoteKey{}, sc)
}

// SpanContextFromContext returns the context of the span in ctx (or of the
// remote parent), and whether there is one.
func SpanContextFromContext(ctx context.Context) (SpanContext, bool) {
	if sp := SpanFromContext(ctx); sp != nil {
		return sp.sc, true
	}
	if ctx != nil {
		if sc, ok := ctx.Value(remoteKey{}).(SpanContext); ok {
			return sc, true
		}
	}
	return SpanContext{}, false
}

// TraceParent returns the traceparent of the span in ctx ("" when none or
// when it is not sampled: there is nothing to link to then).
func TraceParent(ctx context.Context) string {
	sc, ok := SpanContextFromContext(ctx)
	if !ok || !sc.Sampled {
		return ""
	}
	return sc.TraceParent()
}
