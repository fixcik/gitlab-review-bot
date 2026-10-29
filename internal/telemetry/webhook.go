package telemetry

import (
	"bytes"
	"net/http"
)

// WrapWebhook counts every delivery to the webhook handler in
// webhook_events_total without changing the handler. The event comes from
// the X-Gitlab-Event header; the result from the response: 405 ->
// method_not_allowed, 401 -> unauthorized, other 4xx -> bad_request (bad
// JSON, unreadable or too large body), 200 with a body starting with
// "accepted" -> accepted, any other 200 ("ignored: <why>") -> ignored.
// The bodies are the webhook handler's stable plain-text answers
// (webhook.writeOK).
func (m *Metrics) WrapWebhook(h http.Handler) http.Handler {
	if m == nil || h == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &sniffWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		m.WebhookEvents.With(WebhookEvent(r.Header.Get("X-Gitlab-Event")), webhookResult(sw)).Inc()
	})
}

// WebhookEvent maps the X-Gitlab-Event header to merge_request | note |
// pipeline | other.
func WebhookEvent(h string) string {
	switch h {
	case "Merge Request Hook":
		return EventMergeRequest
	case "Note Hook":
		return EventNote
	case "Pipeline Hook":
		return EventPipeline
	default:
		return EventOther
	}
}

func webhookResult(sw *sniffWriter) string {
	status := sw.status
	if status == 0 {
		status = http.StatusOK
	}
	switch {
	case status == http.StatusMethodNotAllowed:
		return WebhookMethodNotAllowed
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return WebhookUnauthorized
	case status >= 400:
		return WebhookBadRequest
	case bytes.HasPrefix(sw.head, []byte("accepted")):
		return WebhookAccepted
	default:
		return WebhookIgnored
	}
}

// sniffWriter keeps the status and the first bytes of the body.
type sniffWriter struct {
	http.ResponseWriter
	status int
	head   []byte
}

const sniffBytes = 16

func (s *sniffWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *sniffWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if n := sniffBytes - len(s.head); n > 0 {
		s.head = append(s.head, b[:min(n, len(b))]...)
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *sniffWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
