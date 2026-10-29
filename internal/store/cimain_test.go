package store

import (
	"context"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"testing"
	"time"
)

func TestCIMain(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	if c, err := s.GetCIMain(ctx, 1, "main"); err != nil || c != nil {
		t.Fatalf("empty: %+v %v", c, err)
	}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	want := core.CIMain{ProjectID: 1, Branch: "main", GreenPipeline: 10, GreenSHA: "aaa", RedPipeline: 12, RedSHA: "bbb",
		CulpritMR: 7, NoteID: 99, LastPipeline: 12, UpdatedAt: at}
	if err := s.SaveCIMain(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCIMain(ctx, 1, "main")
	if err != nil || got == nil || *got != want {
		t.Fatalf("got %+v %v", got, err)
	}
	want.RedPipeline, want.RedSHA, want.CulpritMR, want.NoteID, want.GreenPipeline = 0, "", 0, 0, 13
	if err := s.SaveCIMain(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetCIMain(ctx, 1, "main"); got.GreenPipeline != 13 || got.RedPipeline != 0 {
		t.Fatalf("updated %+v", got)
	}
	if got, _ := s.GetCIMain(ctx, 1, "release"); got != nil {
		t.Fatal("other branch")
	}
}
