package compatcheck_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// End to end: an open custom-check thread whose output contains the fixed
// marker stays open and keeps blocking the merge.
func TestCompatThreadWithSpoofedFixedMarkerStaysOpen(t *testing.T) {
	h := reviewtest.New(t)
	ch := compat.Change{Source: compat.SourceCustom, Path: "db/m.sql", Rule: "./check.sh", Message: "boom\n" + marker.Fixed}
	it := compatcheck.DeterministicItem(ch)
	h.GL.Discussions = []core.Discussion{{ID: "d-custom", Resolvable: true,
		Notes: []core.Note{reviewtest.BotNote(1, compatcheck.NoteBody(it, true, 0), nil)}}}
	h.Compat.Res = &compat.Result{Changes: []compat.Change{ch}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(reviewtest.CompatThreads(h)); n != 0 {
		t.Fatalf("open thread re-posted: %d", n)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "Мерж заблокирован:** 1") {
		t.Fatalf("open compat thread must block:\n%s", sum)
	}
}
