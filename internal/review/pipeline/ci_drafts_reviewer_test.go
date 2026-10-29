package pipeline_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// The CI thread goes through publishDrafts: a leftover draft of an
// interrupted job is deleted, never published together with it.
func TestCIThreadNeverPublishesStaleDrafts(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.Drafts = []reviewtest.DraftNote{{Body: "stale draft"}}
	h.Internals().Pipeline.UpsertCIThread(context.Background(), reviewtest.AutoJob(), nil, "🔴 Почему упал пайплайн")
	if reviewtest.StalePublished(h) {
		t.Fatalf("stale draft published: %+v", h.GL.Published)
	}
	h.GL.Mu.Lock()
	defer h.GL.Mu.Unlock()
	if len(h.GL.Published) != 1 || !strings.Contains(h.GL.Published[0].Body, "Почему упал пайплайн") {
		t.Fatalf("published %+v", h.GL.Published)
	}
}
