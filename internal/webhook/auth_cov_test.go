package webhook

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// trackedBody fails every read; read reports whether it was read at all.
type trackedBody struct{ read bool }

func (b *trackedBody) Read([]byte) (int, error) {
	b.read = true
	return 0, errors.New("connection reset")
}

// The secret is checked before the body is read: a wrong or near-miss
// token is refused without reading anything; with the right token a body
// that cannot be read is a 400.
func TestSecretCheckedBeforeBody(t *testing.T) {
	rec := &recorder{}
	h := newTestHandler(rec, "")
	for _, token := range []string{"", "s3cre", secret + "x", " " + secret, strings.ToUpper(secret), secret + "\x00"} {
		body := &trackedBody{}
		req := httptest.NewRequest(http.MethodPost, "/webhook", io.NopCloser(body))
		req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
		req.Header.Set("X-Gitlab-Token", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized || body.read {
			t.Errorf("token %q: %d read=%v", token, w.Code, body.read)
		}
	}
	body := &trackedBody{}
	req := httptest.NewRequest(http.MethodPost, "/webhook", io.NopCloser(body))
	req.Header.Set("X-Gitlab-Event", "Merge Request Hook")
	req.Header.Set("X-Gitlab-Token", secret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "read error") {
		t.Fatalf("unreadable body: %d %q", w.Code, w.Body.String())
	}
	if len(rec.jobs) != 0 {
		t.Fatalf("jobs = %+v", rec.jobs)
	}
}

// Without a Router the handler uses a default one (no bot user: notes are
// ignored, MR events still route).
func TestNewHandlerDefaultRouter(t *testing.T) {
	rec := &recorder{}
	h := NewHandler(Options{Secret: secret, Enqueue: rec.enqueue})
	if h.(*handler).router == nil {
		t.Fatal("no router")
	}
	if w := do(h, http.MethodPost, "Merge Request Hook", secret, mrHook("open", "opened", false, "", "")); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if len(rec.jobs) != 1 {
		t.Fatalf("jobs = %+v", rec.jobs)
	}
}
