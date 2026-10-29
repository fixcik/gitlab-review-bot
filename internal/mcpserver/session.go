package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Spec describes the run a session belongs to.
type Spec struct {
	ProjectID int64
	MRIID     int64
	HeadSHA   string
	RunID     string
	Tools     []Tool
	// TTL is the hard expiry (the run timeout plus a margin); the session
	// should be closed earlier, when the run ends.
	TTL time.Duration
	// Ctx is the job's context: tool calls run under it (cancellation,
	// diagnostics) and OnCall receives it.
	Ctx context.Context
	// Instructions is the initialize "instructions" text (optional).
	Instructions string
}

// Session is the MCP state of one review run.
type Session struct {
	srv     *Server
	spec    Spec
	hash    [sha256.Size]byte
	key     string
	expires time.Time
	tools   map[string]Tool
	order   []string

	mu     sync.Mutex // serializes tool calls
	calls  int
	closed bool
	// seen: an authenticated request reached the session (the CLI
	// connected: initialize, tools/list, ...).
	seen atomic.Bool
}

// Open registers a session for spec and returns it with its bearer token.
// The token is returned once and kept only as a hash.
func (s *Server) Open(spec Spec) (*Session, string, error) {
	if spec.Ctx == nil {
		spec.Ctx = context.Background()
	}
	if spec.TTL <= 0 {
		return nil, "", errors.New("mcpserver: session without TTL")
	}
	ss := &Session{srv: s, spec: spec, tools: map[string]Tool{}, expires: s.o.Now().Add(spec.TTL)}
	for _, t := range spec.Tools {
		if !toolNameRe.MatchString(t.Name) || t.Handler == nil {
			return nil, "", fmt.Errorf("mcpserver: invalid tool %q", t.Name)
		}
		if _, dup := ss.tools[t.Name]; dup {
			return nil, "", fmt.Errorf("mcpserver: duplicate tool %q", t.Name)
		}
		if len(t.InputSchema) == 0 {
			t.InputSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
		}
		ss.tools[t.Name] = t
		ss.order = append(ss.order, t.Name)
	}
	var raw [32]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read never returns an error (Go 1.24)
	token := hex.EncodeToString(raw[:])
	ss.hash = sha256.Sum256([]byte(token))
	ss.key = hex.EncodeToString(ss.hash[:])
	s.mu.Lock()
	s.pruneLocked()
	s.sessions[ss.key] = ss
	s.mu.Unlock()
	return ss, token, nil
}

// pruneLocked drops expired sessions.
func (s *Server) pruneLocked() {
	now := s.o.Now()
	for k, ss := range s.sessions {
		if !now.Before(ss.expires) {
			delete(s.sessions, k)
		}
	}
}

// Sessions is the number of live sessions (tests, diagnostics).
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return len(s.sessions)
}

// lookup finds the live session of a bearer token (nil when unknown, closed
// or expired). The map is keyed by the token's hash, and the hash is
// compared in constant time again, so neither the lookup nor the compare
// leaks the token through timing.
func (s *Server) lookup(token string) *Session {
	if token == "" {
		return nil
	}
	h := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(h[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[key]
	if ss == nil || subtle.ConstantTimeCompare(ss.hash[:], h[:]) != 1 {
		return nil
	}
	if !s.o.Now().Before(ss.expires) {
		delete(s.sessions, key)
		return nil
	}
	return ss
}

// Close ends the session: its token is rejected from now on. Idempotent.
func (ss *Session) Close() {
	if ss == nil {
		return
	}
	ss.srv.mu.Lock()
	delete(ss.srv.sessions, ss.key)
	ss.srv.mu.Unlock()
	ss.mu.Lock()
	ss.closed = true
	ss.mu.Unlock()
}

// Connected reports whether any authenticated request (initialize,
// tools/list, a notification, ...) reached the session: false after a run
// means the CLI never talked to the server (port not forwarded, firewall,
// wrong URL, a CLI that dropped the server).
func (ss *Session) Connected() bool { return ss != nil && ss.seen.Load() }

// Calls is the number of tools/call requests the session served.
func (ss *Session) Calls() int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.calls
}

// ToolNames are the session's tool names in registration order.
func (ss *Session) ToolNames() []string { return append([]string(nil), ss.order...) }
