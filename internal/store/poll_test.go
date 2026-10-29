package store

import (
	"context"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/events"
)

func TestPollState(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if _, ok, err := s.GetPollCursor(ctx, "g"); ok || err != nil {
		t.Fatalf("empty cursor: ok=%v err=%v", ok, err)
	}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if err := s.SetPollCursor(ctx, "g", at); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPollCursor(ctx, "g", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if c, ok, err := s.GetPollCursor(ctx, "g"); !ok || err != nil || !c.Equal(at.Add(time.Minute)) {
		t.Fatalf("cursor %v %v %v", c, ok, err)
	}
	if r, err := s.GetPollMR(ctx, 1, 2); r != nil || err != nil {
		t.Fatalf("unknown mr: %+v %v", r, err)
	}
	want := events.PollMR{ProjectID: 1, IID: 2, State: "opened", Draft: true, SHA: "abc", Title: "feat: x",
		UpdatedAt: at, LastNoteID: 77, NotesSince: at.Add(-time.Hour)}
	if err := s.SavePollMR(ctx, want); err != nil {
		t.Fatal(err)
	}
	want.Draft, want.SHA = false, "def"
	if err := s.SavePollMR(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPollMR(ctx, 1, 2)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != want.State || got.Draft || got.SHA != "def" || got.Title != want.Title || !got.UpdatedAt.Equal(at) ||
		got.LastNoteID != 77 || !got.NotesSince.Equal(want.NotesSince) {
		t.Errorf("got %+v", got)
	}
}
