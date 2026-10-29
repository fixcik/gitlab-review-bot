// Package notify serves the notifications WebSocket (GET /ws/notifications).
// rb-web connects to it and listens for "user.created".
package notify

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// Hub keeps the connected clients.
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

// NewHub returns an empty Hub.
func NewHub() *Hub { return &Hub{clients: map[chan []byte]struct{}{}} }

// Broadcast sends {"event": ..., "data": ...} to every client.
func (h *Hub) Broadcast(event string, payload any) {
	msg, err := json.Marshal(map[string]any{"event": event, "data": payload})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c <- msg:
		default: // slow client: drop the message
		}
	}
}

// ServeWS upgrades the connection. The framing is done by the WebSocket
// library in production; here only the handshake check is kept.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return
	}
	c := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
	}()
	<-r.Context().Done()
}
