package users

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"gitlab.local/reviewbot-sandbox/rb-backend/internal/events"
	"gitlab.local/reviewbot-sandbox/rb-backend/internal/httpx"
)

// Repository is what the handler needs from the store.
type Repository interface {
	Get(ctx context.Context, id string) (User, error)
	List(ctx context.Context, limit int) ([]User, error)
	Create(ctx context.Context, req CreateRequest) (User, error)
}

// Notifier pushes notifications to connected WebSocket clients.
type Notifier interface {
	Broadcast(event string, payload any)
}

// Handler serves /api/v1/users.
type Handler struct {
	repo   Repository
	pub    events.Publisher
	notify Notifier
}

// NewHandler returns a Handler.
func NewHandler(repo Repository, pub events.Publisher, n Notifier) *Handler {
	return &Handler{repo: repo, pub: pub, notify: n}
}

// Register adds the routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/users/{id}", h.get)
	mux.HandleFunc("GET /api/v1/users", h.list)
	mux.HandleFunc("POST /api/v1/users", h.create)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	u, err := h.repo.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 500 {
			httpx.WriteError(w, http.StatusBadRequest, "limit must be 1..500")
			return
		}
		limit = n
	}
	list, err := h.repo.List(r.Context(), limit)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, list)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if !strings.Contains(req.Email, "@") {
		httpx.WriteError(w, http.StatusBadRequest, "email is required")
		return
	}
	u, err := h.repo.Create(r.Context(), req)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.pub.Publish(r.Context(), events.UserCreated{ID: u.ID, Email: u.Email, Name: u.Name}); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "event not published")
		return
	}
	h.notify.Broadcast("user.created", u)
	httpx.WriteJSON(w, http.StatusCreated, u)
}
