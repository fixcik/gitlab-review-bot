package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// NewProxy is the exported constructor: the same CONNECT-only policy.
func TestNewProxyRejectsNonConnect(t *testing.T) {
	h := NewProxy([]string{"example.com"}, nil)
	if !h.(*Proxy).Allowed("example.com") || h.(*Proxy).Allowed("evil.com") {
		t.Fatal("allowlist not applied")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodConnect {
		t.Fatalf("status=%d allow=%q", w.Code, w.Header().Get("Allow"))
	}
}

// connectReq is a CONNECT request as the server sees it (Host may be empty
// for an absolute-form request line; the target is then in URL.Host).
func connectReq(host, urlHost string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	r.Method = http.MethodConnect
	r.Host = host
	r.URL = &url.URL{Host: urlHost}
	return r
}

// countingProxy is a proxy for example.com whose dialer returns the conns
// handed out by up (nil: a failing dial) and counts the dials.
func countingProxy(up func() net.Conn) (*Proxy, *atomic.Int32) {
	var n atomic.Int32
	p := newProxy([]string{"example.com"}, nil)
	p.Dial = func(context.Context, string, string) (net.Conn, error) {
		n.Add(1)
		if up == nil {
			return nil, errors.New("no dial expected")
		}
		return up(), nil
	}
	return p, &n
}

// The target comes from URL.Host when Host is empty, and the policy applies
// to it; a target without a port is refused; nothing is dialed for a denied
// request or when the connection cannot be hijacked.
func TestServeHTTPTargetChecks(t *testing.T) {
	p, dials := countingProxy(nil)
	for _, tc := range []struct {
		r      *http.Request
		code   int
		reason string
	}{
		{connectReq("", "evil.com:443"), http.StatusForbidden, "host not in allowlist"},
		{connectReq("", "example.com"), http.StatusForbidden, "bad target"},
		{connectReq("example.com:443", "evil.com:443"), http.StatusInternalServerError, "hijacking not supported"},
		{connectReq("", "example.com:443"), http.StatusInternalServerError, "hijacking not supported"},
	} {
		w := httptest.NewRecorder() // not a Hijacker
		p.ServeHTTP(w, tc.r)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.reason) {
			t.Errorf("%s/%s: %d %q, want %d %q", tc.r.Host, tc.r.URL.Host, w.Code, w.Body.String(), tc.code, tc.reason)
		}
	}
	if dials.Load() != 0 {
		t.Fatalf("dialed %d times", dials.Load())
	}
}

// hijackWriter is a ResponseWriter whose Hijack returns conn/brw or err.
type hijackWriter struct {
	*httptest.ResponseRecorder
	conn net.Conn
	brw  *bufio.ReadWriter
	err  error
}

func (h *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, h.brw, h.err
}

// closedPeer reports whether c's peer was closed (a read returns EOF/an
// error instead of blocking).
func closedPeer(t *testing.T, c net.Conn) bool {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

// Every failure after the upstream dial closes the upstream connection (and
// the client's): a failed hijack, a client gone before the 200, an upstream
// gone before the pipelined client bytes.
func TestServeHTTPFailuresAfterDialCloseUpstream(t *testing.T) {
	newBrw := func(buffered string) *bufio.ReadWriter {
		r := bufio.NewReader(strings.NewReader(buffered))
		if buffered != "" {
			r.Peek(1) // fill the buffer, like the server's read buffer
		}
		return bufio.NewReadWriter(r, bufio.NewWriter(io.Discard))
	}

	t.Run("hijack fails", func(t *testing.T) {
		up, upPeer := net.Pipe()
		defer upPeer.Close()
		p, _ := countingProxy(func() net.Conn { return up })
		p.ServeHTTP(&hijackWriter{ResponseRecorder: httptest.NewRecorder(), err: errors.New("no")}, connectReq("example.com:443", ""))
		if !closedPeer(t, upPeer) {
			t.Error("upstream left open")
		}
	})

	t.Run("client gone", func(t *testing.T) {
		up, upPeer := net.Pipe()
		defer upPeer.Close()
		client, clientPeer := net.Pipe()
		clientPeer.Close() // the 200 cannot be written
		p, _ := countingProxy(func() net.Conn { return up })
		p.ServeHTTP(&hijackWriter{ResponseRecorder: httptest.NewRecorder(), conn: client, brw: newBrw("")}, connectReq("example.com:443", ""))
		if !closedPeer(t, upPeer) {
			t.Error("upstream left open")
		}
	})

	t.Run("upstream gone with pipelined bytes", func(t *testing.T) {
		up, upPeer := net.Pipe()
		upPeer.Close() // forwarding the buffered ClientHello fails
		client, clientPeer := net.Pipe()
		defer clientPeer.Close()
		var got []byte
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _ = io.ReadAll(clientPeer)
		}()
		p, _ := countingProxy(func() net.Conn { return up })
		p.ServeHTTP(&hijackWriter{ResponseRecorder: httptest.NewRecorder(), conn: client, brw: newBrw("CLIENTHELLO")}, connectReq("example.com:443", ""))
		wg.Wait() // returns only when the proxy closed the client side
		if string(got) != "HTTP/1.1 200 Connection Established\r\n\r\n" {
			t.Errorf("client got %q", got)
		}
	})
}

// fakeListener hands out conns, then fails with err (after Close: ErrClosed).
type fakeListener struct {
	conns  chan net.Conn
	err    error
	closed chan struct{}
	once   sync.Once
}

func newFakeListener(err error, conns ...net.Conn) *fakeListener {
	l := &fakeListener{conns: make(chan net.Conn, len(conns)), err: err, closed: make(chan struct{})}
	for _, c := range conns {
		l.conns <- c
	}
	return l
}

func (l *fakeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	default:
	}
	if l.err != nil {
		return nil, l.err
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *fakeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

// A listener failure other than shutdown ends Serve with that error.
func TestForwarderServeListenerError(t *testing.T) {
	boom := errors.New("accept: too many open files")
	f := &Forwarder{Target: "t:1"}
	if err := f.Serve(context.Background(), newFakeListener(boom)); !errors.Is(err, boom) {
		t.Fatalf("Serve = %v", err)
	}
}

// A connection accepted while the forwarder is shutting down is closed
// without dialing the target.
func TestForwarderNoDialAfterShutdown(t *testing.T) {
	c, peer := net.Pipe()
	defer peer.Close()
	var dials atomic.Int32
	f := &Forwarder{Target: "t:1", Dial: func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("unexpected")
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.Serve(ctx, newFakeListener(nil, c)); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if dials.Load() != 0 {
		t.Error("target dialed after shutdown")
	}
	if !closedPeer(t, peer) {
		t.Error("connection left open")
	}
}

// When the target stops accepting data the client connection is closed.
func TestForwarderTargetWriteFails(t *testing.T) {
	up, upPeer := net.Pipe()
	upPeer.Close()
	c, peer := net.Pipe()
	// A long idle timeout: the connection must close because the write
	// failed, not because it went idle.
	f := &Forwarder{Target: "t:1", IdleTimeout: 30 * time.Second, Dial: func(context.Context, string, string) (net.Conn, error) {
		return up, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	ln := newFakeListener(nil, c)
	go func() { done <- f.Serve(ctx, ln) }()
	go peer.Write([]byte("request")) // read by the forwarder, write to the target fails
	if !closedPeer(t, peer) {
		t.Error("client connection left open")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

// IP literals are refused even when the allowlist names them: only host
// names are tunnelled.
func TestIPLiteralRefusedEvenIfListed(t *testing.T) {
	p, dials := countingProxy(nil)
	p.exact["10.0.0.1"] = true
	p.exact["::1"] = true
	for _, target := range []string{"10.0.0.1:443", "[::1]:443"} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, connectReq(target, ""))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "IP literals not allowed") {
			t.Errorf("%s: %d %q", target, w.Code, w.Body.String())
		}
	}
	if dials.Load() != 0 {
		t.Fatal("dialed an IP literal")
	}
}

// Over the cap a connection is closed at once (not left hanging); once a
// slot is free the forwarder serves again and relays every byte both ways.
func TestForwarderCapRefusesThenServes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	var dials atomic.Int32
	f := &Forwarder{Target: "t:1", MaxConns: 1, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			<-block
			return nil, errors.New("down")
		}
		a, b := net.Pipe()
		go func() { // echo
			defer b.Close()
			io.Copy(b, b)
		}()
		return a, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Serve(ctx, ln)
	dial := func() net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		return c
	}
	c1 := dial()
	for dials.Load() == 0 { // c1 holds the only slot
		time.Sleep(5 * time.Millisecond)
	}
	c2 := dial()
	if _, err := c2.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("over the cap: %v, want EOF", err)
	}
	close(block)
	if _, err := c1.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("target down: %v, want EOF", err)
	}
	// The slot is released just after c1 is closed: retry briefly.
	echo := func(c net.Conn) error {
		for _, msg := range []string{"x", "yz"} {
			if _, err := c.Write([]byte(msg)); err != nil {
				return err
			}
			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(c, buf); err != nil {
				return err
			}
			if string(buf) != msg {
				return fmt.Errorf("echo %q: got %q", msg, buf)
			}
		}
		return nil
	}
	var lastErr error
	for i := 0; i < 50; i++ {
		if lastErr = echo(dial()); lastErr == nil {
			if n := dials.Load(); n != 2 {
				t.Fatalf("%d dials, want 2 (the refused connection is never relayed)", n)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("not served after the slot was freed: %v", lastErr)
}
