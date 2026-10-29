package gitlab

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Error is a non-2xx response from GitLab. Body is truncated and never
// contains request credentials.
type Error struct {
	Method     string
	Path       string // request path without query
	StatusCode int
	Body       string

	retryAfter time.Duration // parsed Retry-After, for 429/5xx
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("gitlab: %s %s: HTTP %d", e.Method, e.Path, e.StatusCode)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Unwrap maps 404 to core.ErrNotFound and 403 to core.ErrForbidden so
// errors.Is works.
func (e *Error) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return core.ErrNotFound
	case http.StatusForbidden:
		return core.ErrForbidden
	}
	return nil
}

// StatusCode returns the HTTP status of a GitLab error, or 0.
func StatusCode(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.StatusCode
	}
	return 0
}

type scrubbed struct{ msg string }

func (s scrubbed) Error() string { return s.msg }

// minRedactLen is the shortest token that is redacted from error text. The
// token is only ever sent in the PRIVATE-TOKEN header, so redaction is a
// last line of defence; replacing a very short "token" (e.g. "x" in tests or
// a misconfiguration) would corrupt unrelated text such as URLs.
const minRedactLen = 8

// redact replaces the token in s, unless the token is too short to be a
// meaningful secret.
func redact(s, token string) string {
	if len(token) < minRedactLen {
		return s
	}
	return strings.ReplaceAll(s, token, "[REDACTED]")
}

// scrub removes the token from a transport error message (it should never be
// there, but URLs in errors are a classic leak).
func scrub(err error, token string) error {
	if len(token) < minRedactLen || !strings.Contains(err.Error(), token) {
		return err
	}
	return scrubbed{redact(err.Error(), token)}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
