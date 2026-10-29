// Package welcome sends a welcome email for every user.created event of
// rb-backend.
package welcome

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// Event is the envelope published by rb-backend (internal/events).
type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// UserCreated mirrors rb-backend's events.UserCreated.
type UserCreated struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// Mailer sends emails.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// LogMailer logs instead of sending (development).
type LogMailer struct{ Logger *slog.Logger }

// Send implements Mailer.
func (m LogMailer) Send(_ context.Context, to, subject, _ string) error {
	m.Logger.Info("mail", "to", to, "subject", subject)
	return nil
}

// Handler handles user events.
type Handler struct{ mailer Mailer }

// NewHandler returns a Handler.
func NewHandler(m Mailer) *Handler { return &Handler{mailer: m} }

// Handle processes one raw message; other event types are ignored.
func (h *Handler) Handle(ctx context.Context, msg []byte) error {
	var e Event
	if err := json.Unmarshal(msg, &e); err != nil {
		return fmt.Errorf("decode event: %w", err)
	}
	if e.Type != "user.created" {
		return nil
	}
	var u UserCreated
	if err := json.Unmarshal(e.Data, &u); err != nil {
		return fmt.Errorf("decode user.created: %w", err)
	}
	if u.Email == "" {
		return fmt.Errorf("user.created %s without email", u.ID)
	}
	name := u.Name
	if name == "" {
		name = "there"
	}
	return h.mailer.Send(ctx, u.Email, "Welcome!", fmt.Sprintf("Hi %s, welcome aboard.", name))
}
