package jira

import (
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
)

// Error is a non-2xx response. Body is truncated and never contains the token.
type Error struct {
	Path       string
	StatusCode int
	Body       string

	retryAfter time.Duration
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("jira: GET %s: HTTP %d", e.Path, e.StatusCode)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Unwrap maps statuses to ErrNotFound / ErrForbidden.
func (e *Error) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrForbidden
	}
	return nil
}

func (c *Client) endpoint(q url.Values, segs ...string) *url.URL {
	u := *c.base
	raw := c.base.EscapedPath()
	for _, s := range segs {
		raw += "/" + url.PathEscape(s)
	}
	p, err := url.PathUnescape(raw)
	if err != nil {
		p = raw
	}
	u.Path, u.RawPath = p, raw
	if q != nil {
		u.RawQuery = q.Encode()
	}
	return &u
}

func (c *Client) getJSON(ctx context.Context, u *url.URL, out any) error {
	for attempt := 0; ; attempt++ {
		body, err := c.once(ctx, u)
		if err == nil {
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("jira: decode response: %w", err)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		retryAfter, ok := retryable(err)
		if !ok || attempt >= c.maxRetries {
			return err
		}
		delay := c.backoff(attempt, retryAfter)
		c.log.Debug("jira: retrying request", "path", u.Path, "attempt", attempt+1, "delay", delay, "err", err)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func retryable(err error) (time.Duration, bool) {
	var je *Error
	if errors.As(err, &je) {
		if je.StatusCode == http.StatusTooManyRequests || je.StatusCode >= 500 {
			return je.retryAfter, true
		}
		return 0, false
	}
	var tb *tooBig
	if errors.As(err, &tb) {
		return 0, false
	}
	return 0, true // transport error
}

func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(retryAfter, maxRetryDelay)
	}
	d := c.baseDelay << attempt
	if d <= 0 {
		return 0
	}
	d += time.Duration(rand.Int64N(int64(d)/5 + 1))
	return min(d, maxRetryDelay)
}

type tooBig struct{ path string }

func (e *tooBig) Error() string { return "jira: response too large: " + e.path }

type scrubbed struct{ msg string }

func (s scrubbed) Error() string { return s.msg }

func (c *Client) once(ctx context.Context, u *url.URL) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("jira: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "gitlab-review-bot")
	resp, err := c.hc.Do(req)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, c.token) {
			return nil, fmt.Errorf("jira: GET %s: %w", u.Path, scrubbed{c.redact(msg)})
		}
		return nil, fmt.Errorf("jira: GET %s: %w", u.Path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("jira: GET %s: read body: %w", u.Path, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if len(data) > maxBodyBytes {
			return nil, &tooBig{u.Path}
		}
		return data, nil
	}
	body := c.redact(strings.TrimSpace(string(data)))
	if len(body) > maxErrorBody {
		body = body[:maxErrorBody] + "...(truncated)"
	}
	je := &Error{Path: u.Path, StatusCode: resp.StatusCode, Body: body}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		je.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	return nil, je
}

func (c *Client) redact(s string) string {
	if len(c.token) < 8 {
		return s
	}
	return strings.ReplaceAll(s, c.token, "[REDACTED]")
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(max(secs, 0)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
