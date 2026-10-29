// Package mcpserver is the bot's MCP server: the review model (`claude -p`
// in the sandbox) calls bot-side tools during a review over the MCP
// "Streamable HTTP" transport (JSON-RPC 2.0, one POST with a JSON request,
// one JSON response; no SSE streams).
//
// Every review run gets its own Session: a random 256-bit bearer token bound
// to the run (project, MR, head SHA, run id), its own tool set, a call cap
// and a hard expiry. The token is the only credential: an unknown, closed or
// expired token gets 401 before anything else is looked at. Tools only see
// what the review package gave the session (data of this MR and of the
// repositories this run may read); nothing here writes to GitLab.
//
// The server is served on its own listener (review.mcp.listen), never on the
// bot's public HTTP mux.
package mcpserver

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ServerName is the MCP server name the CLI is configured with; Claude Code
// names the tools mcp__<ServerName>__<tool>.
const ServerName = "reviewbot"

// Path is where the server answers.
const Path = "/mcp"

// Defaults of Options.
const (
	DefaultMaxCalls         = 200
	DefaultMaxResponseBytes = 64 << 10
	DefaultMaxRequestBytes  = 1 << 20
	DefaultCallTimeout      = 60 * time.Second
	// DefaultFallbackCooldown is how long new review runs skip MCP after
	// a run whose CLI did not connect (Server.Fallback).
	DefaultFallbackCooldown = 30 * time.Minute
)

// Call statuses (CallInfo.Status, telemetry label).
const (
	StatusOK      = "ok"      // the tool answered
	StatusError   = "error"   // the tool answered with isError (bad input, refused)
	StatusLimit   = "limit"   // the run's call cap was reached
	StatusUnknown = "unknown" // no such tool in this session
)

// Options configures the server.
type Options struct {
	// MaxCalls caps tools/call per session (default DefaultMaxCalls).
	MaxCalls int
	// MaxResponseBytes caps the text of one tool result; longer text is cut
	// with a note (default DefaultMaxResponseBytes).
	MaxResponseBytes int
	// MaxRequestBytes caps a request body (default DefaultMaxRequestBytes).
	MaxRequestBytes int64
	// CallTimeout bounds one tool call (default DefaultCallTimeout).
	CallTimeout time.Duration
	// Version is reported in serverInfo.
	Version string
	Logger  *slog.Logger
	Now     func() time.Time
	// OnCall is called after every tools/call with the session's Ctx.
	OnCall func(ctx context.Context, c CallInfo)
	// FallbackCooldown: how long Degraded reports true after a Fallback
	// (default DefaultFallbackCooldown).
	FallbackCooldown time.Duration
	// OnFallback is called by every Fallback with its reason (metrics).
	OnFallback func(reason string)
}

// Server holds the live sessions. It is safe for concurrent use.
type Server struct {
	o        Options
	mu       sync.Mutex
	sessions map[string]*Session // by hex(sha256(token))
	// degradedUntil: new runs skip MCP before this time (Fallback).
	degradedUntil time.Time
}

// New returns a server with defaults applied.
func New(o Options) *Server {
	if o.MaxCalls <= 0 {
		o.MaxCalls = DefaultMaxCalls
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if o.MaxRequestBytes <= 0 {
		o.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.FallbackCooldown <= 0 {
		o.FallbackCooldown = DefaultFallbackCooldown
	}
	return &Server{o: o, sessions: map[string]*Session{}}
}

// Fallback records that a review run could not use the server (its CLI did
// not connect): Degraded reports true for FallbackCooldown, so the next runs
// go without MCP instead of failing the same way (a circuit breaker per
// process). reason goes to OnFallback.
func (s *Server) Fallback(reason string) {
	s.mu.Lock()
	s.degradedUntil = s.o.Now().Add(s.o.FallbackCooldown)
	until := s.degradedUntil
	s.mu.Unlock()
	s.o.Logger.Warn("mcp: review run fell back to no MCP; new runs skip MCP until the cooldown ends",
		"reason", reason, "until", until.Format(time.RFC3339))
	if f := s.o.OnFallback; f != nil {
		f(reason)
	}
}

// Degraded reports whether new runs should skip MCP (a Fallback less than
// FallbackCooldown ago) and until when.
func (s *Server) Degraded() (bool, time.Time) {
	if s == nil {
		return false, time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.o.Now().Before(s.degradedUntil), s.degradedUntil
}
