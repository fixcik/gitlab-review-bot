package tracing

import (
	"context"
	"log/slog"
	"sync"
)

// Log correlation: LogHandler adds trace_id and span_id to every log
// record written within a traced unit of work. Most of the bot logs
// without a context (log.Info, not log.InfoContext), so a record without a
// span in its context is matched by its "mr" attribute (the MR key the
// review flow logs) against the jobs running right now (Active): the queue
// runs at most one job per MR at a time, so the key identifies the job.

// Active maps keys (MR keys) to the root span of the job running for them.
// The zero value is ready to use; a nil *Active is empty.
type Active struct {
	mu sync.Mutex
	m  map[string]SpanContext
}

// Register records sc under key until the returned function is called.
func (a *Active) Register(key string, sc SpanContext) (unregister func()) {
	if a == nil || key == "" || !sc.IsValid() {
		return func() {}
	}
	a.mu.Lock()
	if a.m == nil {
		a.m = map[string]SpanContext{}
	}
	a.m[key] = sc
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		if cur, ok := a.m[key]; ok && cur == sc {
			delete(a.m, key)
		}
		a.mu.Unlock()
	}
}

// Lookup returns the span context registered under key.
func (a *Active) Lookup(key string) (SpanContext, bool) {
	if a == nil || key == "" {
		return SpanContext{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	sc, ok := a.m[key]
	return sc, ok
}

// Log attribute keys added by LogHandler.
const (
	LogTraceID = "trace_id"
	LogSpanID  = "span_id"
)

// LogHandler wraps next: records get trace_id/span_id of the span in their
// context, or of the active job whose key equals the record's keyAttr
// attribute ("mr"). Only sampled traces are referenced (an id that never
// reaches the backend is noise). active may be nil (context only).
func LogHandler(next slog.Handler, active *Active, keyAttr string) slog.Handler {
	return &logHandler{next: next, active: active, keyAttr: keyAttr}
}

type logHandler struct {
	next    slog.Handler
	active  *Active
	keyAttr string
	key     string // keyAttr value from WithAttrs
	grouped bool   // WithGroup was called: attrs added later land in a group
}

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	sc, ok := SpanContextFromContext(ctx)
	if !ok || !sc.Sampled {
		ok = false
		key := h.key
		if key == "" && h.keyAttr != "" && !h.grouped {
			r.Attrs(func(a slog.Attr) bool {
				if v, ok := h.keyOf(a); ok {
					key = v
					return false
				}
				return true
			})
		}
		if key != "" {
			sc, ok = h.active.Lookup(key)
		}
	}
	if ok && sc.Sampled && !h.grouped {
		r = r.Clone()
		r.AddAttrs(slog.String(LogTraceID, sc.TraceID.String()), slog.String(LogSpanID, sc.SpanID.String()))
	}
	return h.next.Handle(ctx, r)
}

// keyOf returns the value of a string keyAttr attribute.
func (h *logHandler) keyOf(a slog.Attr) (string, bool) {
	if a.Key == h.keyAttr && a.Value.Kind() == slog.KindString {
		return a.Value.String(), true
	}
	return "", false
}

func (h *logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	n.next = h.next.WithAttrs(as)
	if !h.grouped {
		for _, a := range as {
			if v, ok := h.keyOf(a); ok {
				n.key = v
			}
		}
	}
	return &n
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	n := *h
	n.next = h.next.WithGroup(name)
	if name != "" {
		n.grouped = true
	}
	return &n
}
