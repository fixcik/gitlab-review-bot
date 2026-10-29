package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// setup starts a TLS "upstream" (cert valid for example.com) and a proxy
// whose dialer sends every allowed connection to that upstream.
func setup(t *testing.T, allow ...string) (*httptest.Server, *httptest.Server, *[]string) {
	t.Helper()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from "+r.Host)
	}))
	t.Cleanup(upstream.Close)

	var mu sync.Mutex
	var dialed []string
	p := newProxy(allow, nil)
	p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	proxy := httptest.NewServer(p)
	t.Cleanup(proxy.Close)
	return upstream, proxy, &dialed
}

func clientVia(t *testing.T, upstream, proxy *httptest.Server) *http.Client {
	pu, _ := url.Parse(proxy.URL)
	base := upstream.Client().Transport.(*http.Transport)
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(pu),
			TLSClientConfig: base.TLSClientConfig.Clone(),
		},
	}
}

func TestAllowedTunnel(t *testing.T) {
	upstream, proxy, dialed := setup(t, "example.com", "*.anthropic.com")
	c := clientVia(t, upstream, proxy)
	resp, err := c.Get("https://example.com/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "hello from example.com" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if len(*dialed) != 1 || (*dialed)[0] != "example.com:443" {
		t.Fatalf("dialed = %v", *dialed)
	}
}

func connect(t *testing.T, proxy *httptest.Server, target string) int {
	t.Helper()
	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestDenied(t *testing.T) {
	_, proxy, dialed := setup(t, "api.anthropic.com", "*.example.org")
	cases := map[string]int{
		"evil.com:443":           403,
		"api.anthropic.com:80":   403,
		"api.anthropic.com:22":   403,
		"anthropic.com:443":      403,
		"xapi.anthropic.com:443": 403,
		"example.org:443":        403, // wildcard does not match apex
		"127.0.0.1:443":          403,
		"[::1]:443":              403,
		"gitlab.internal:443":    403,
		"api.anthropic.com.:443": 200, // trailing dot normalised
		"API.Anthropic.COM:443":  200,
		"a.b.example.org:443":    200,
	}
	for target, want := range cases {
		if got := connect(t, proxy, target); got != want {
			t.Errorf("CONNECT %s = %d, want %d", target, got, want)
		}
	}
	for _, d := range *dialed {
		if !strings.HasSuffix(d, ":443") || strings.HasPrefix(d, "evil") {
			t.Errorf("unexpected dial %s", d)
		}
	}
}

func TestPlainHTTPRejected(t *testing.T) {
	_, proxy, dialed := setup(t, "example.com")
	pu, _ := url.Parse(proxy.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := c.Get("http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Direct request to the proxy itself.
	resp, err = http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(*dialed) != 0 {
		t.Fatalf("dialed = %v", *dialed)
	}
}

func TestDeniedHTTPSViaClient(t *testing.T) {
	upstream, proxy, _ := setup(t, "api.anthropic.com")
	c := clientVia(t, upstream, proxy)
	_, err := c.Get("https://example.com/")
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpstreamUnreachable(t *testing.T) {
	p := newProxy([]string{"example.com"}, nil)
	p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: io.ErrUnexpectedEOF}
	}
	proxy := httptest.NewServer(p)
	defer proxy.Close()
	if got := connect(t, proxy, "example.com:443"); got != http.StatusBadGateway {
		t.Fatalf("status = %d", got)
	}
}

func TestIdleTimeout(t *testing.T) {
	// Upstream that accepts and never speaks.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	p := newProxy([]string{"example.com"}, nil)
	p.IdleTimeout = 200 * time.Millisecond
	p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}
	proxy := httptest.NewServer(p)
	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("connect: %v %v", resp, err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, err = br.ReadByte()
	if err == nil {
		t.Fatal("expected tunnel to close")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("tunnel was not closed by idle timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("idle close took too long")
	}
}

func TestAllowedMatcher(t *testing.T) {
	p := newProxy([]string{" API.anthropic.com ", "*.Example.com", ""}, nil)
	for host, want := range map[string]bool{
		"api.anthropic.com": true,
		"anthropic.com":     false,
		"a.example.com":     true,
		"a.b.example.com":   true,
		"example.com":       false,
		"badexample.com":    false,
		"":                  false,
	} {
		if got := p.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v", host, got)
		}
	}
}
