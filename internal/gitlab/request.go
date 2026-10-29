package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// seg escapes one path segment, including '/', so GitLab sees it as a
// single parameter (e.g. file paths in the repository files API).
func seg(s string) string {
	return url.PathEscape(s) // escapes '/' as %2F and ' ' as %20
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

// endpoint builds the full URL from already-escaped path segments.
func (c *Client) endpoint(q url.Values, segs ...string) *url.URL {
	u := *c.base
	raw := c.base.EscapedPath()
	for _, s := range segs {
		raw += "/" + s
	}
	p, err := url.PathUnescape(raw)
	if err != nil {
		p = raw
	}
	u.Path = p
	u.RawPath = raw // keeps %2F intact when it differs from Path
	if q != nil {
		u.RawQuery = q.Encode()
	}
	return &u
}

type request struct {
	method string
	u      *url.URL
	body   any // JSON-encoded when non-nil
	// retryServerErrors: retry on 5xx and transport errors. False for
	// non-idempotent POSTs (only 429 is retried then).
	retryServerErrors bool
	// maxBytes caps a successful response body (0 = no cap); a larger body
	// fails with ErrTooLarge.
	maxBytes int64
	// tail > 0 keeps only the last tail bytes of a successful body (job
	// logs); at most maxTailRead bytes are read, a longer body keeps the end
	// of what was read. maxBytes is ignored then.
	tail int64
}

// maxTailRead bounds how much of a body a tail request reads.
const maxTailRead = 64 << 20

// ErrTooLarge is returned when a response body exceeds request.maxBytes; it
// is core.ErrTooLarge (callers match the core sentinel).
var ErrTooLarge = core.ErrTooLarge

type response struct {
	header http.Header
	body   []byte
}

func (c *Client) do(ctx context.Context, r request) (*response, error) {
	if err := c.checkWrite(r); err != nil {
		return nil, err
	}
	var payload []byte
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, fmt.Errorf("gitlab: encode request: %w", err)
		}
		payload = b
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, r, payload, attempt)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		retryAfter, retryable := c.retryable(r, err)
		if !retryable || attempt >= c.maxRetries {
			return nil, err
		}
		delay := c.backoff(attempt, retryAfter)
		c.log.Debug("gitlab: retrying request",
			"method", r.method, "path", r.u.Path, "attempt", attempt+1, "delay", delay, "err", err)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func (c *Client) retryable(r request, err error) (time.Duration, bool) {
	if errors.Is(err, ErrTooLarge) || errors.Is(err, errTooManyRedirects) {
		return 0, false
	}
	var ge *Error
	if errors.As(err, &ge) {
		switch {
		case ge.StatusCode == http.StatusTooManyRequests:
			return ge.retryAfter, true
		case ge.StatusCode >= 500:
			return ge.retryAfter, r.retryServerErrors
		default:
			return 0, false
		}
	}
	// Transport error (connection refused/reset, timeout).
	return 0, r.retryServerErrors
}

func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, maxRetryDelay)
	}
	d := c.baseDelay << attempt
	if d <= 0 {
		return 0
	}
	// up to 20% jitter
	d += time.Duration(rand.Int64N(int64(d)/5 + 1))
	return min(d, maxRetryDelay)
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// once makes one attempt. Inside a traced job it is a client span (see
// trace.go): method, templated path, status; never the query or headers.
func (c *Client) once(ctx context.Context, r request, payload []byte, attempt int) (out *response, err error) {
	ctx, sp := c.startSpan(ctx, r, attempt)
	code := 0
	defer func() { endSpan(sp, code, err) }()
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("gitlab: build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	c.observe(r.method, resp, err)
	if err != nil {
		return nil, fmt.Errorf("gitlab: %s %s: %w", r.method, r.u.Path, scrub(err, c.token))
	}
	code = resp.StatusCode
	defer resp.Body.Close()
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	if r.tail > 0 && success {
		data, err := readTail(io.LimitReader(resp.Body, maxTailRead), r.tail)
		if err != nil {
			return nil, fmt.Errorf("gitlab: %s %s: read body: %w", r.method, r.u.Path, err)
		}
		return &response{header: resp.Header, body: data}, nil
	}
	var rd io.Reader = resp.Body
	if r.maxBytes > 0 {
		rd = io.LimitReader(resp.Body, r.maxBytes+1)
	}
	data, err := io.ReadAll(rd)
	if err != nil {
		return nil, fmt.Errorf("gitlab: %s %s: read body: %w", r.method, r.u.Path, err)
	}
	if success {
		if r.maxBytes > 0 && int64(len(data)) > r.maxBytes {
			return nil, fmt.Errorf("%w: %s %s is larger than %d bytes", ErrTooLarge, r.method, r.u.Path, r.maxBytes)
		}
		return &response{header: resp.Header, body: data}, nil
	}
	ge := &Error{
		Method:     r.method,
		Path:       r.u.Path,
		StatusCode: resp.StatusCode,
		Body:       truncate(redact(string(data), c.token), maxErrorBody),
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		ge.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	return nil, ge
}

// readTail reads rd to the end and returns its last n bytes.
func readTail(rd io.Reader, n int64) ([]byte, error) {
	buf := make([]byte, 0, min(n*2, 1<<20))
	chunk := make([]byte, 32<<10)
	for {
		k, err := rd.Read(chunk)
		buf = append(buf, chunk[:k]...)
		if int64(len(buf)) > 2*n {
			buf = append(buf[:0], buf[int64(len(buf))-n:]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if int64(len(buf)) > n {
		buf = buf[int64(len(buf))-n:]
	}
	return buf, nil
}

func (c *Client) getJSON(ctx context.Context, u *url.URL, out any) error {
	resp, err := c.do(ctx, request{method: http.MethodGet, u: u, retryServerErrors: true})
	if err != nil {
		return err
	}
	return decode(resp.body, out)
}

func decode(b []byte, out any) error {
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("gitlab: decode response: %w", err)
	}
	return nil
}

// list fetches all pages of a list endpoint (X-Next-Page pagination).
func list[T any](ctx context.Context, c *Client, q url.Values, segs ...string) ([]T, error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("per_page", strconv.Itoa(perPage))
	var all []T
	page := "1"
	for i := 0; i < maxPages; i++ {
		q.Set("page", page)
		resp, err := c.do(ctx, request{method: http.MethodGet, u: c.endpoint(q, segs...), retryServerErrors: true})
		if err != nil {
			return nil, err
		}
		var items []T
		if err := decode(resp.body, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		next := strings.TrimSpace(resp.header.Get("X-Next-Page"))
		if next == "" || next == page || len(items) == 0 {
			return all, nil
		}
		page = next
	}
	return nil, fmt.Errorf("gitlab: too many pages for %s", strings.Join(segs, "/"))
}
