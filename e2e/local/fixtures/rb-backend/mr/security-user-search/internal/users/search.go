package users

import (
	"context"
	"fmt"
	"net/http"

	"gitlab.local/reviewbot-sandbox/rb-backend/internal/httpx"
)

// Search returns users whose name contains q.
func (s *Store) Search(ctx context.Context, q string) ([]User, error) {
	query := fmt.Sprintf("SELECT id, email, name, created_at FROM users WHERE name ILIKE '%%%s%%' ORDER BY name LIMIT 50", q)
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SearchHandler serves GET /api/v1/users/search?q=.
func SearchHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			httpx.WriteError(w, http.StatusBadRequest, "q is required")
			return
		}
		list, err := store.Search(r.Context(), q)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, list)
	}
}
