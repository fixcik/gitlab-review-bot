package egress

// TCP forwarder for the bot's MCP endpoint (review.mcp). The sandbox
// network is internal: its only reachable container is this proxy. To let
// the review model call the bot's MCP tools without attaching the bot to
// the sandbox network (where its public HTTP port would be reachable too),
// the proxy forwards ONE port to ONE fixed target (`reviewbot egress
// -mcp-forward reviewbot:8091`). It does not parse the traffic: the MCP
// server authenticates every request with the run's bearer token.

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Forwarder relays TCP connections from a listener to one fixed target.
type Forwarder struct {
	Target string // host:port
	Log    *slog.Logger
	// Dial is used for the target connection; tests override it.
	Dial DialFunc
	// MaxConns caps concurrent connections (default 64).
	MaxConns int
	// IdleTimeout closes a connection idle in both directions (default
	// 5 minutes; a tool call is bounded by the MCP server well below it).
	IdleTimeout time.Duration
}

// Serve accepts connections on ln until ctx is done or ln fails.
func (f *Forwarder) Serve(ctx context.Context, ln net.Listener) error {
	if f.Log == nil {
		f.Log = slog.New(slog.DiscardHandler)
	}
	if f.Dial == nil {
		d := &net.Dialer{Timeout: dialTimeout}
		f.Dial = d.DialContext
	}
	if f.MaxConns <= 0 {
		f.MaxConns = 64
	}
	if f.IdleTimeout <= 0 {
		f.IdleTimeout = 5 * time.Minute
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	slots := make(chan struct{}, f.MaxConns)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			f.Log.Warn("mcp forward: too many connections, refused", "from", c.RemoteAddr().String())
			_ = c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			f.relay(ctx, c)
		}()
	}
}

func (f *Forwarder) relay(ctx context.Context, c net.Conn) {
	defer c.Close()
	if ctx.Err() != nil {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	up, err := f.Dial(dctx, "tcp", f.Target)
	cancel()
	if err != nil {
		f.Log.Warn("mcp forward: target unreachable", "target", f.Target, "err", err)
		return
	}
	defer up.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close(); _ = up.Close() }) // shutdown
	defer stop()
	pipe(c, up, f.IdleTimeout)
}

// pipe copies both ways until one side closes or nothing moves for idle.
func pipe(a, b net.Conn, idle time.Duration) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
			n, err := src.Read(buf)
			if n > 0 {
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if tc, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	// Give the other direction a moment to drain its response, then stop.
	t := time.NewTimer(idle)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}
