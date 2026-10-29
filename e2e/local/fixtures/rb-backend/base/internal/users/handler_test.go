package users

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.local/reviewbot-sandbox/rb-backend/internal/events"
)

type fakeRepo struct{ users map[string]User }

func (f *fakeRepo) Get(_ context.Context, id string) (User, error) {
	u, ok := f.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (f *fakeRepo) List(_ context.Context, limit int) ([]User, error) {
	var out []User
	for _, u := range f.users {
		if len(out) == limit {
			break
		}
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeRepo) Create(_ context.Context, req CreateRequest) (User, error) {
	u := User{ID: "u2", Email: req.Email, Name: req.Name, CreatedAt: time.Unix(0, 0)}
	f.users[u.ID] = u
	return u, nil
}

type nopPublisher struct{ got []events.UserCreated }

func (p *nopPublisher) Publish(_ context.Context, e events.UserCreated) error {
	p.got = append(p.got, e)
	return nil
}

type nopNotifier struct{}

func (nopNotifier) Broadcast(string, any) {}

func newMux(t *testing.T) (*http.ServeMux, *nopPublisher) {
	t.Helper()
	repo := &fakeRepo{users: map[string]User{"u1": {ID: "u1", Email: "a@example.com", Name: "A"}}}
	pub := &nopPublisher{}
	mux := http.NewServeMux()
	NewHandler(repo, pub, nopNotifier{}).Register(mux)
	return mux, pub
}

func TestGetUser(t *testing.T) {
	mux, _ := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users/u1", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"email":"a@example.com"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing user: got %d", rec.Code)
	}
}

func TestCreateUserPublishesEvent(t *testing.T) {
	mux, pub := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/users",
		strings.NewReader(`{"email":"b@example.com","name":"B"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if len(pub.got) != 1 || pub.got[0].Email != "b@example.com" {
		t.Fatalf("events: %+v", pub.got)
	}
}

func TestCreateUserValidatesEmail(t *testing.T) {
	mux, _ := newMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"name":"B"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
}
