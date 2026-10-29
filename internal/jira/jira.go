// Package jira is a minimal read-only client for Jira Data Center / Server
// (REST API v2, Personal Access Token). It fetches what the review bot needs
// to compare an MR with its task: summary, description, status, type, the
// latest comments, the parent (sub-tasks) and the epic.
package jira

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	defaultMaxRetries    = 3
	defaultBaseDelay     = 500 * time.Millisecond
	maxRetryDelay        = 30 * time.Second
	maxBodyBytes         = 4 << 20
	maxErrorBody         = 300
	defaultCommentsLimit = 10
	defaultEpicField     = "customfield_10008"
	// MaxTextRunes caps descriptions and comment bodies.
	MaxTextRunes = 8000
)

var (
	// ErrNotFound: the issue does not exist (HTTP 404).
	ErrNotFound = errors.New("jira: issue not found")
	// ErrForbidden: the token is invalid or has no access (HTTP 401/403).
	ErrForbidden = errors.New("jira: access denied")
)

// Issue is a Jira issue with the context the reviewer uses.
type Issue struct {
	Key         string
	URL         string // <jira>/browse/KEY
	Summary     string
	Description string
	Status      string
	Type        string
	Comments    []Comment // latest comments, oldest first
	Parent      *IssueBrief
	EpicKey     string
	Epic        *IssueBrief
}

// IssueBrief is a parent or epic: summary and description only.
type IssueBrief struct {
	Key         string
	URL         string
	Summary     string
	Description string
}

// Comment is one issue comment.
type Comment struct {
	Author  string // display name
	Created time.Time
	Body    string
}

// Client talks to one Jira instance. It is safe for concurrent use.
type Client struct {
	base          *url.URL // without trailing slash
	token         string
	hc            *http.Client
	log           *slog.Logger
	caFile        string
	maxRetries    int
	baseDelay     time.Duration
	commentsLimit int
	epicField     string
}

// Option configures a Client.
type Option func(*Client) error

// WithHTTPClient uses hc for requests.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) error {
		if hc == nil {
			return errors.New("jira: nil http client")
		}
		c.hc = hc
		return nil
	}
}

// WithCAFile adds PEM certificates for a Jira behind a private CA.
func WithCAFile(path string) Option {
	return func(c *Client) error { c.caFile = path; return nil }
}

// WithLogger sets the logger for retry diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) error {
		if l != nil {
			c.log = l
		}
		return nil
	}
}

// WithRetry overrides the retry policy (mostly for tests).
func WithRetry(maxRetries int, baseDelay time.Duration) Option {
	return func(c *Client) error {
		c.maxRetries = max(maxRetries, 0)
		c.baseDelay = baseDelay
		return nil
	}
}

// WithCommentsLimit sets how many latest comments GetIssue returns
// (0 = none).
func WithCommentsLimit(n int) Option {
	return func(c *Client) error { c.commentsLimit = max(n, 0); return nil }
}

// WithEpicLinkField sets the Epic Link custom field ("customfield_10008").
// Empty disables epic lookup.
func WithEpicLinkField(field string) Option {
	return func(c *Client) error {
		field = strings.TrimSpace(field)
		if field != "" && !fieldRe.MatchString(field) {
			return fmt.Errorf("jira: invalid epic link field %q", field)
		}
		c.epicField = field
		return nil
	}
}

var fieldRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// New creates a client for the Jira at baseURL (e.g. https://jira.example.com,
// a context path such as /jira is kept).
func New(baseURL, token string, opts ...Option) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("jira: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("jira: base URL must be http(s)://host, got %q", baseURL)
	}
	if token == "" {
		return nil, errors.New("jira: empty token")
	}
	u.RawQuery, u.Fragment, u.RawFragment, u.User = "", "", "", nil
	u.Path, u.RawPath = strings.TrimRight(u.Path, "/"), ""
	c := &Client{
		base:          u,
		token:         token,
		log:           slog.New(slog.DiscardHandler),
		maxRetries:    defaultMaxRetries,
		baseDelay:     defaultBaseDelay,
		commentsLimit: defaultCommentsLimit,
		epicField:     defaultEpicField,
	}
	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}
	if c.hc == nil {
		c.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if c.caFile != "" {
		if err := c.applyCA(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Client) applyCA() error {
	pem, err := os.ReadFile(c.caFile)
	if err != nil {
		return fmt.Errorf("jira: read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("jira: no certificates found in %s", c.caFile)
	}
	var tr *http.Transport
	switch t := c.hc.Transport.(type) {
	case nil:
		tr = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		tr = t.Clone()
	default:
		return errors.New("jira: WithCAFile requires an *http.Transport")
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig.RootCAs = pool
	hc := *c.hc
	hc.Transport = tr
	c.hc = &hc
	return nil
}

// BrowseURL is the web link of an issue.
func (c *Client) BrowseURL(key string) string {
	return c.base.String() + "/browse/" + url.PathEscape(key)
}
