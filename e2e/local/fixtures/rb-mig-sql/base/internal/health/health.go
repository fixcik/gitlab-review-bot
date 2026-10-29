// Package health reports service status.
package health

import (
	"encoding/json"
	"net/http"
)

// Handler returns {"status":"ok"}.
func Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
