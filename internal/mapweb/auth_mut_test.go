package mapweb

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Only GET/HEAD reach the page; a refusal never carries map data.
func TestMethodsAndRefusalHaveNoData(t *testing.T) {
	h := newHandler(Options{Enabled: true, Token: "s3cret"})
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		r := httptest.NewRequest(m, DataPath, nil)
		r.Header.Set("Authorization", "Bearer s3cret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" || strings.Contains(w.Body.String(), "g/orders") {
			t.Errorf("%s: %d allow=%q %q", m, w.Code, w.Header().Get("Allow"), w.Body)
		}
	}
	for _, p := range []string{PagePath, DataPath, PagePath + "?token=nope"} {
		w := get(h, p, nil)
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "g/orders") || strings.Contains(w.Body.String(), "/orders") {
			t.Errorf("%s: %d %q", p, w.Code, w.Body)
		}
	}
	if w := get(h, DataPath, map[string]string{"Authorization": "Bearer s3cret"}); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "g/orders") {
		t.Fatalf("authorized data: %d", w.Code)
	}
}
