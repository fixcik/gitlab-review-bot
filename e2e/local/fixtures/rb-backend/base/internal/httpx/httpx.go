// Package httpx holds small HTTP helpers shared by the handlers.
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON writes v as a JSON responce with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error is the error body of every endpoint.
type Error struct {
	Message string `json:"message"`
}

// WriteError writes an Error body.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, Error{Message: msg})
}

// RegisterHealth adds GET /health.
func RegisterHealth(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}
