package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// RequestIDHeader is the header carrying the request id.
const RequestIDHeader = "X-Request-ID"

// WithRequestID copies X-Request-ID from the request (or generates one) into
// the response.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r)
	})
}
