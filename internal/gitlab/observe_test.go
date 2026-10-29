package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestObserverSeesRetries(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"id":1,"username":"bot"}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "tok", WithRetry(3, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []int
	c.SetObserver(func(method string, status int) {
		mu.Lock()
		defer mu.Unlock()
		if method != http.MethodGet {
			t.Errorf("method %s", method)
		}
		seen = append(seen, status)
	})
	if _, _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != 502 || seen[1] != 502 || seen[2] != 200 {
		t.Fatalf("seen %v", seen)
	}
	seen = nil
	srv.Close()
	mu.Unlock()
	if _, _, err := c.CurrentUser(context.Background()); err == nil {
		t.Fatal("want transport error")
	}
	mu.Lock()
	if len(seen) == 0 || seen[0] != 0 {
		t.Fatalf("transport error should be status 0: %v", seen)
	}
	c.SetObserver(nil)
}
