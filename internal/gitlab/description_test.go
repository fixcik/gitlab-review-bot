package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// The MR description update sends only the description, needs
// WriteDescription and is never retried on a server error (the text may
// already be saved).
func TestUpdateMRDescription(t *testing.T) {
	var put map[string]any
	calls := 0
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		put = nil
		json.NewDecoder(r.Body).Decode(&put)
		writeJSON(w, map[string]any{"iid": 7, "description": put["description"]})
	})
	if err := c.UpdateMRDescription(context.Background(), 42, 7, "text"); !errors.Is(err, core.ErrForbidden) || calls != 0 {
		t.Fatalf("comments-only client changed the description: %v", err)
	}
	for _, p := range []WritePolicy{BotWrites, ServiceWrites} {
		c.writes = p
		if err := c.UpdateMRDescription(context.Background(), 42, 7, "новый\n<!-- x -->"); err != nil {
			t.Fatalf("policy %b: %v", p, err)
		}
	}
	if len(put) != 1 || put["description"] != "новый\n<!-- x -->" {
		t.Fatalf("PUT body must carry only the description: %v", put)
	}
	if req := f.req[0]; req.Method != http.MethodPut || req.RawPath != "/api/v4/projects/42/merge_requests/7" {
		t.Fatalf("request %+v", req)
	}
	if writeAllowedWithBody(BotWrites, http.MethodPut, "/projects/1/merge_requests/2/merge", descriptionUpdate{Description: "x"}) {
		t.Fatal("description body allowed on /merge")
	}
	if err := c.UpdateMRDescription(context.Background(), 42, 7, strings.Repeat("x", MaxDescriptionBytes+1)); err == nil {
		t.Fatal("an oversized description must be refused before sending")
	}
}
