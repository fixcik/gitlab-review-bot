package users

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"gitlab.local/reviewbot-sandbox/rb-backend/internal/httpx"
)

// SessionStore reads login sessions.
type SessionStore struct {
	db *sql.DB
}

// NewSessionStore returns a SessionStore on db.
func NewSessionStore(db *sql.DB) *SessionStore { return &SessionStore{db: db} }

// ListByUser returns the active sessions of a user.
func (s *SessionStore) ListByUser(ctx context.Context, userID string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_agent, started_at FROM sessions WHERE user_id = $1 AND ended_at IS NULL`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.UserAgent, &s.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SessionsHandler serves GET /api/v1/users/{id}/sessions.
func SessionsHandler(store *SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := store.ListByUser(r.Context(), r.PathValue("id"))
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, list)
	}
}
