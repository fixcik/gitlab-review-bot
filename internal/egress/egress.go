// Package egress is a CONNECT-only HTTPS forward proxy with a host
// allowlist. It is the single way out of the internal docker network the
// Claude sandbox containers run on.
//
// Only "CONNECT host:443" to an allowlisted host is tunnelled. TLS is not
// terminated: the proxy sees only the target host name, never the traffic.
package egress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	dialTimeout = 10 * time.Second
	// idleTimeout closes a tunnel when neither side sent anything for this
	// long. Claude streams responses, so long-running requests keep the
	// connection busy; a truly idle keep-alive connection is dropped.
	idleTimeout = 10 * time.Minute
)

// DialFunc dials the upstream connection.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Proxy is the http.Handler returned by NewProxy.
type Proxy struct {
	exact    map[string]bool
	suffixes []string // ".example.com" for "*.example.com"
	log      *slog.Logger
	// Dial is used for upstream connections; tests override it.
	Dial DialFunc
	// AllowedPorts defaults to {443}.
	AllowedPorts map[string]bool
	IdleTimeout  time.Duration
}

// NewProxy returns a CONNECT-only proxy that lets through port 443 on hosts
// matching allow: exact names, or "*.example.com" for any subdomain of
// example.com (not example.com itself).
func NewProxy(allow []string, logger *slog.Logger) http.Handler {
	return newProxy(allow, logger)
}

func newProxy(allow []string, logger *slog.Logger) *Proxy {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	p := &Proxy{
		exact:        map[string]bool{},
		log:          logger,
		AllowedPorts: map[string]bool{"443": true},
		IdleTimeout:  idleTimeout,
	}
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	p.Dial = d.DialContext
	for _, a := range allow {
		a = normalizeHost(a)
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "*.") {
			p.suffixes = append(p.suffixes, a[1:])
		} else {
			p.exact[a] = true
		}
	}
	return p
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// Allowed reports whether host (without port) is on the allowlist.
func (p *Proxy) Allowed(host string) bool {
	host = normalizeHost(host)
	switch {
	case host == "":
		return false
	case p.exact[host]:
		return true
	}
	for _, s := range p.suffixes {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return true
		}
	}
	return false
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	client := r.RemoteAddr
	if r.Method != http.MethodConnect {
		p.log.Warn("egress denied", "client", client, "method", r.Method, "target", r.Host+r.URL.Path, "reason", "not CONNECT")
		w.Header().Set("Allow", http.MethodConnect)
		http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
		return
	}
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		p.deny(w, client, target, "bad target")
		return
	}
	if !p.AllowedPorts[port] {
		p.deny(w, client, target, "port not allowed")
		return
	}
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		p.deny(w, client, target, "IP literals not allowed")
		return
	}
	if !p.Allowed(host) {
		p.deny(w, client, target, "host not in allowlist")
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	dialCtx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	upstream, err := p.Dial(dialCtx, "tcp", net.JoinHostPort(normalizeHost(host), port))
	cancel()
	if err != nil {
		p.log.Warn("egress dial failed", "client", client, "target", target, "err", err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		p.log.Error("egress hijack failed", "err", err)
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		conn.Close()
		upstream.Close()
		return
	}
	// Bytes the client sent right after the CONNECT headers (e.g. a TLS
	// ClientHello) may already sit in the server's read buffer.
	if n := brw.Reader.Buffered(); n > 0 {
		buf, _ := brw.Peek(n)
		if _, err := upstream.Write(buf); err != nil {
			conn.Close()
			upstream.Close()
			return
		}
	}
	p.log.Info("egress allowed", "client", client, "target", target)
	start := time.Now()
	up, down := p.pipe(conn, upstream)
	p.log.Info("egress tunnel closed", "client", client, "target", target,
		"bytes_up", up, "bytes_down", down, "duration", time.Since(start).Round(time.Millisecond))
}

func (p *Proxy) deny(w http.ResponseWriter, client, target, reason string) {
	p.log.Warn("egress denied", "client", client, "target", target, "reason", reason)
	http.Error(w, "forbidden by egress policy: "+reason, http.StatusForbidden)
}

// pipe copies in both directions until one side closes or the tunnel is
// idle for IdleTimeout; returns bytes client->upstream and upstream->client.
func (p *Proxy) pipe(client, upstream net.Conn) (int64, int64) {
	idle := p.IdleTimeout
	if idle <= 0 {
		idle = idleTimeout
	}
	var (
		wg       sync.WaitGroup
		up, down int64
		once     sync.Once
	)
	closeBoth := func() {
		once.Do(func() {
			client.Close()
			upstream.Close()
		})
	}
	cp := func(dst, src net.Conn, n *int64) {
		defer wg.Done()
		*n, _ = io.Copy(&idleWriter{Conn: dst, idle: idle}, &idleReader{Conn: src, peer: dst, idle: idle})
		// Half-close if supported so the other direction can drain;
		// otherwise close both.
		if tc, ok := dst.(interface{ CloseWrite() error }); ok {
			if err := tc.CloseWrite(); err == nil {
				return
			}
		}
		closeBoth()
	}
	wg.Add(2)
	go cp(upstream, client, &up)
	go cp(client, upstream, &down)
	wg.Wait()
	closeBoth()
	return up, down
}

// idleReader extends both connections' deadlines on every read, so a tunnel
// is torn down only when the whole tunnel is idle.
type idleReader struct {
	net.Conn
	peer net.Conn
	idle time.Duration
}

func (r *idleReader) Read(b []byte) (int, error) {
	_ = r.SetReadDeadline(time.Now().Add(r.idle))
	n, err := r.Conn.Read(b)
	if n > 0 {
		_ = r.peer.SetReadDeadline(time.Now().Add(r.idle))
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return n, io.EOF
	}
	return n, err
}

// idleWriter bounds every write by the idle timeout.
type idleWriter struct {
	net.Conn
	idle time.Duration
}

func (w *idleWriter) Write(b []byte) (int, error) {
	_ = w.SetWriteDeadline(time.Now().Add(w.idle))
	return w.Conn.Write(b)
}
