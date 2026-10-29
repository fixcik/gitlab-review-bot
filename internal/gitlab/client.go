// Package gitlab is a small hand-written GitLab REST API v4 client that
// implements core.GitLab.
package gitlab

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

const (
	defaultMaxRetries = 3
	defaultBaseDelay  = 500 * time.Millisecond
	maxRetryDelay     = 60 * time.Second
	perPage           = 100
	maxErrorBody      = 512
	maxPages          = 1000
)

// Client is a GitLab REST v4 client. It is safe for concurrent use.
type Client struct {
	base       *url.URL // ends with /api/v4
	token      string
	hc         *http.Client
	log        *slog.Logger
	maxRetries int
	baseDelay  time.Duration
	caFile     string
	userAgent  string
	writes     WritePolicy
	observer   atomic.Pointer[Observer] // see observe.go
}

var _ core.GitLab = (*Client)(nil)

// Option configures a Client.
type Option func(*Client) error

// WithHTTPClient uses hc for all requests. WithCAFile is applied on top of
// hc's transport when it is an *http.Transport (or nil).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) error {
		if hc == nil {
			return errors.New("gitlab: nil http client")
		}
		c.hc = hc
		return nil
	}
}

// WithCAFile adds the PEM certificates in path to the system roots, for
// self-managed GitLab instances behind a private CA.
func WithCAFile(path string) Option {
	return func(c *Client) error {
		c.caFile = path
		return nil
	}
}

// WithLogger sets the logger used for retry diagnostics.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) error {
		if l != nil {
			c.log = l
		}
		return nil
	}
}

// WithRetry overrides the retry policy (number of retries after the first
// attempt, and the base delay of the exponential backoff). Mostly for tests.
func WithRetry(maxRetries int, baseDelay time.Duration) Option {
	return func(c *Client) error {
		if maxRetries < 0 {
			maxRetries = 0
		}
		c.maxRetries = maxRetries
		c.baseDelay = baseDelay
		return nil
	}
}

// New creates a client for the GitLab instance at baseURL (e.g.
// "https://gitlab.example.com"; a trailing "/api/v4" is accepted).
func New(baseURL, token string, opts ...Option) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("gitlab: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("gitlab: base URL must be http(s)://host, got %q", baseURL)
	}
	if token == "" {
		return nil, errors.New("gitlab: empty token")
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	p := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(p, "/api/v4") {
		p += "/api/v4"
	}
	u.Path, u.RawPath = p, ""

	c := &Client{
		base:       u,
		token:      token,
		log:        slog.New(slog.DiscardHandler),
		maxRetries: defaultMaxRetries,
		baseDelay:  defaultBaseDelay,
		userAgent:  "gitlab-review-bot",
		writes:     WriteMRComments,
	}
	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}
	if c.hc == nil {
		c.hc = &http.Client{Timeout: 60 * time.Second}
	}
	if c.hc.CheckRedirect == nil {
		// On a copy: a caller's client shared with another Client (another
		// GitLab host) must not keep this client's redirect policy.
		hc := *c.hc
		hc.CheckRedirect = c.checkRedirect
		c.hc = &hc
	}
	if c.caFile != "" {
		if err := c.applyCA(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// checkRedirect keeps the token on the GitLab host only. Go copies custom
// headers such as PRIVATE-TOKEN to every redirect target, and GitLab
// redirects artifact (and LFS/upload) downloads to object storage with a
// presigned URL that needs no token: the token must not leave GitLab.
// errTooManyRedirects stops a redirect loop. It is not retried: the same
// request would loop again.
var errTooManyRedirects = errors.New("gitlab: stopped after 10 redirects")

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errTooManyRedirects
	}
	if !strings.EqualFold(req.URL.Host, c.base.Host) || req.URL.Scheme != c.base.Scheme {
		req.Header.Del("PRIVATE-TOKEN")
		req.Header.Del("Authorization")
	}
	return nil
}

func (c *Client) applyCA() error {
	pem, err := os.ReadFile(c.caFile)
	if err != nil {
		return fmt.Errorf("gitlab: read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("gitlab: no certificates found in %s", c.caFile)
	}
	var tr *http.Transport
	switch t := c.hc.Transport.(type) {
	case nil:
		tr = http.DefaultTransport.(*http.Transport).Clone()
	case *http.Transport:
		tr = t.Clone()
	default:
		return errors.New("gitlab: WithCAFile requires an *http.Transport")
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig.RootCAs = pool
	hc := *c.hc // don't mutate a caller-owned client
	hc.Transport = tr
	c.hc = &hc
	return nil
}
