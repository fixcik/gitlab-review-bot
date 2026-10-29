package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestForwarderRelaysHTTP(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Write([]byte(r.Method + " " + r.URL.Path + " auth=" + r.Header.Get("Authorization") + " body=" + string(b)))
	}))
	defer backend.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	f := &Forwarder{Target: strings.TrimPrefix(backend.URL, "http://")}
	go func() { done <- f.Serve(ctx, ln) }()

	for i := 0; i < 3; i++ { // keep-alive and fresh connections
		req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/mcp", strings.NewReader(`{"jsonrpc":"2.0"}`))
		req.Header.Set("Authorization", "Bearer t")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != `POST /mcp auth=Bearer t body={"jsonrpc":"2.0"}` {
			t.Errorf("relayed: %q", b)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forwarder did not stop")
	}
}

func TestForwarderTargetDownAndCap(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	block := make(chan struct{})
	f := &Forwarder{Target: "unreachable:1", MaxConns: 1, Dial: func(ctx context.Context, _, addr string) (net.Conn, error) {
		if addr != "unreachable:1" {
			t.Errorf("dialed %q", addr)
		}
		<-block
		return nil, io.EOF
	}}
	go f.Serve(ctx, ln)
	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	time.Sleep(50 * time.Millisecond) // c1 holds the only slot
	c2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := bufio.NewReader(c2).ReadByte(); err == nil {
		t.Error("second connection over the cap was served")
	}
	close(block)
	_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := bufio.NewReader(c1).ReadByte(); err == nil {
		t.Error("connection with an unreachable target stayed open")
	}
}
