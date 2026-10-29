package welcome

import (
	"context"
	"testing"
)

type captureMailer struct{ to, body string }

func (c *captureMailer) Send(_ context.Context, to, _, body string) error {
	c.to, c.body = to, body
	return nil
}

func TestHandleUserCreated(t *testing.T) {
	m := &captureMailer{}
	err := NewHandler(m).Handle(context.Background(),
		[]byte(`{"type":"user.created","data":{"id":"u1","email":"a@example.com","name":"Ann"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.to != "a@example.com" || m.body != "Hi Ann, welcome aboard." {
		t.Fatalf("got %q %q", m.to, m.body)
	}
}
