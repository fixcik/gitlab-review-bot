package gitlab

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestDiscussionResolvedBy(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id": "d1", "notes": [
			{"id": 1, "body": "x", "author": {"id": 9, "username": "bot"}, "created_at": "2026-01-02T03:04:05Z",
			 "resolvable": true, "resolved": true, "resolved_by": {"id": 5, "username": "alice"}},
			{"id": 2, "body": "y", "author": {"id": 5, "username": "alice"}, "created_at": "2026-01-02T03:04:05Z",
			 "resolvable": true, "resolved": true, "resolved_by": null}]}`)
	})
	d, err := c.GetDiscussion(context.Background(), 1, 2, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Resolved || d.Notes[0].ResolvedByID != 5 || d.Notes[0].ResolvedByUsername != "alice" || d.Notes[1].ResolvedByID != 0 {
		t.Fatalf("resolved_by: %+v", d)
	}
}
