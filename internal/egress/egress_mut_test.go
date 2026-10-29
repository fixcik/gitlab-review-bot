package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// rawUpstream starts a TCP listener whose connections are handled by fn and
// a proxy (allowing example.com) that dials every tunnel to it.
func rawUpstream(t *testing.T, idle time.Duration, fn func(net.Conn)) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); fn(c) }()
		}
	}()
	p := newProxy([]string{"example.com"}, nil)
	p.IdleTimeout = idle
	p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}
	proxy := httptest.NewServer(p)
	t.Cleanup(proxy.Close)
	return proxy
}

// Bytes the client pipelines right behind the CONNECT headers (a TLS
// ClientHello in practice) sit in the server's read buffer and must still be
// forwarded upstream.
func TestPipelinedBytesForwarded(t *testing.T) {
	proxy := rawUpstream(t, time.Minute, func(c net.Conn) {
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		c.Write(append([]byte("echo:"), buf...))
	})
	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	// One write: headers and payload arrive together.
	io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\nHELLO")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("connect: %v %v", resp, err)
	}
	got := make([]byte, 10)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("pipelined bytes were not forwarded: %v", err)
	}
	if string(got) != "echo:HELLO" {
		t.Fatalf("got %q", got)
	}
}

// A tunnel that is busy in one direction only (Claude streaming a long
// response while the client sends nothing) must not be torn down by the idle
// timeout: traffic either way keeps the whole tunnel alive. The upstream
// closes as soon as it sees EOF from the client, as TLS servers do.
func TestOneWayStreamKeepsTunnelAlive(t *testing.T) {
	const idle = 300 * time.Millisecond
	proxy := rawUpstream(t, idle, func(c net.Conn) {
		eof := make(chan struct{})
		go func() { io.Copy(io.Discard, c); close(eof) }()
		for i := 0; i < 10; i++ {
			select {
			case <-eof:
				return
			case <-time.After(100 * time.Millisecond):
			}
			if _, err := c.Write([]byte{'x'}); err != nil {
				return
			}
		}
		c.Write([]byte{'.'})
	})
	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("connect: %v %v", resp, err)
	}
	body, _ := io.ReadAll(br)
	if string(body) != "xxxxxxxxxx." {
		t.Fatalf("stream cut short: %q (tunnel closed while one direction was busy)", body)
	}
}

// A zero IdleTimeout means "use the default", not "expire immediately".
func TestZeroIdleTimeoutUsesDefault(t *testing.T) {
	proxy := rawUpstream(t, 0, func(c net.Conn) {
		time.Sleep(50 * time.Millisecond)
		c.Write([]byte("late"))
	})
	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("connect: %v %v", resp, err)
	}
	body, _ := io.ReadAll(br)
	if string(body) != "late" {
		t.Fatalf("got %q", body)
	}
}

// The wildcard suffix itself (".example.com", an empty leftmost label) is
// not a subdomain and must not match.
func TestWildcardBareSuffixDenied(t *testing.T) {
	p := newProxy([]string{"*.example.com"}, nil)
	if p.Allowed(".example.com") {
		t.Fatal(`Allowed(".example.com") = true`)
	}
}

// When the client half-closes its side (done sending), the tunnel must keep
// the other direction open until the upstream has finished answering.
func TestHalfCloseDrainsResponse(t *testing.T) {
	proxy := rawUpstream(t, time.Minute, func(c net.Conn) {
		req, _ := io.ReadAll(c) // until the client's EOF
		time.Sleep(50 * time.Millisecond)
		c.Write(append([]byte("got:"), req...))
	})
	conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("connect: %v %v", resp, err)
	}
	io.WriteString(conn, "ping")
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(br)
	if string(body) != "got:ping" {
		t.Fatalf("got %q (response lost after client half-close)", body)
	}
}
