package tracing

import (
	"net/http"
	"strconv"
)

// TraceParentHeader is the W3C trace context request header.
const TraceParentHeader = "traceparent"

// Handler wraps an HTTP handler: every request is a server span named
// "<METHOD> <route>" with http.request.method, http.route, url.path (no
// query) and http.response.status_code; 5xx marks the span as an error.
// The span is always the root of a new trace sampled by the tracer's own
// ratio: the request is not authenticated yet (the webhook secret is checked
// inside h), so a valid incoming traceparent header is only a span link —
// its sampled flag and trace id must not force sampling past sample_ratio.
// extra may add attributes from the request (never its body or secret
// headers). With a nil Tracer h is returned unchanged.
func (t *Tracer) Handler(route string, h http.Handler, extra func(*http.Request) []Attr) http.Handler {
	if t == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attrs := []Attr{String("http.request.method", r.Method), String("http.route", route), String("url.path", r.URL.Path),
			String("url.scheme", scheme(r))}
		if extra != nil {
			attrs = append(attrs, extra(r)...)
		}
		opts := []StartOption{WithNewRoot(), WithKind(KindServer), WithAttrs(attrs...)}
		if sc, err := ParseTraceParent(r.Header.Get(TraceParentHeader)); err == nil {
			opts = append(opts, WithLinks(Link{SpanContext: sc}))
		}
		ctx, sp := t.Start(r.Context(), r.Method+" "+route, opts...)
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				sp.SetAttr(Int("http.response.status_code", http.StatusInternalServerError))
				sp.SetStatus(StatusError, "panic")
				sp.End()
				panic(p)
			}
			sp.SetAttr(Int("http.response.status_code", sw.code))
			if sw.code >= 500 {
				sp.SetAttr(String("error.type", strconv.Itoa(sw.code)))
				sp.SetStatus(StatusError, "HTTP "+strconv.Itoa(sw.code))
			}
			sp.End()
		}()
		h.ServeHTTP(sw, r.WithContext(ctx))
	})
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

type statusWriter struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.code, w.wroteHeader = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
