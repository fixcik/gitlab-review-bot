package gitlab

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// startSpan starts the client span of one HTTP attempt (a no-op outside a
// traced job). The name is "<METHOD> <template>" with ids, project paths,
// SHAs and file paths replaced by placeholders, so it stays low-cardinality
// and carries no file names; url.path is the same template. The query
// (where GitLab also accepts tokens) is never recorded.
func (c *Client) startSpan(ctx context.Context, r request, attempt int) (context.Context, *tracing.Span) {
	if tracing.SpanFromContext(ctx) == nil {
		return ctx, nil
	}
	tmpl := URLTemplate(r.u.EscapedPath())
	attrs := []tracing.Attr{
		tracing.String("http.request.method", r.method),
		tracing.String("url.template", tmpl),
		tracing.String("url.path", tmpl),
		tracing.String("server.address", c.base.Hostname()),
		tracing.String("url.scheme", c.base.Scheme),
	}
	if port := c.base.Port(); port != "" {
		if n, err := strconv.Atoi(port); err == nil {
			attrs = append(attrs, tracing.Int("server.port", n))
		}
	}
	if attempt > 0 {
		attrs = append(attrs, tracing.Int("http.request.resend_count", attempt))
	}
	return tracing.Start(ctx, r.method+" "+tmpl, tracing.WithKind(tracing.KindClient), tracing.WithAttrs(attrs...))
}

func endSpan(sp *tracing.Span, r request, code int, err error) {
	if sp == nil {
		return
	}
	if code != 0 {
		sp.SetAttr(tracing.Int("http.response.status_code", code))
	}
	switch {
	case isMissingFile(r, code, err):
		// The file is simply not there (optional files such as
		// .reviewbot.yaml): an answer, not a failure. The status stays.
	case code >= 400:
		sp.SetAttr(tracing.String("error.type", strconv.Itoa(code)))
		sp.SetStatus(tracing.StatusError, "HTTP "+strconv.Itoa(code))
	case err != nil:
		sp.RecordError(err, errorClass(err))
	}
	sp.End()
}

// isMissingFile reports a completed 404 answer of the repository files API:
// callers ask for optional files, so the span of that answer is not an error.
// A 404 of any other endpoint, any other status, and a response whose body
// could not be read (err is not the typed *Error) still are.
func isMissingFile(r request, code int, err error) bool {
	var ge *Error
	return code == http.StatusNotFound && r.method == http.MethodGet &&
		strings.Contains(r.u.EscapedPath(), "/repository/files/") &&
		errors.As(err, &ge)
}

// errorClass is the error.type of a request that got no HTTP status.
func errorClass(err error) string {
	switch {
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "network"
	}
}

// URLTemplate turns an escaped API path into a template:
// /api/v4/projects/42/merge_requests/7/notes ->
// /projects/{id}/merge_requests/{id}/notes. Numeric segments and project
// paths (group%2Fapp) become {id}, 40-hex SHAs {sha}, the file path of the
// repository files API {path}; the /api/v4 prefix is dropped.
func URLTemplate(escaped string) string {
	p := escaped
	if i := strings.Index(p, "/api/v4"); i >= 0 {
		p = p[i+len("/api/v4"):]
	}
	orig := strings.Split(p, "/")
	segs := append([]string(nil), orig...)
	for i, s := range orig {
		prev := ""
		if i > 0 {
			prev = orig[i-1]
		}
		switch {
		case s == "":
		case prev == "files" || prev == "blobs":
			segs[i] = "{path}"
		case isDigits(s), strings.Contains(strings.ToLower(s), "%2f"):
			segs[i] = "{id}"
		case len(s) == 40 && isHex(s):
			segs[i] = "{sha}"
		case prev == "discussions" || prev == "users" || prev == "groups":
			segs[i] = "{id}"
		}
	}
	out := strings.Join(segs, "/")
	if out == "" {
		return "/"
	}
	return out
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
