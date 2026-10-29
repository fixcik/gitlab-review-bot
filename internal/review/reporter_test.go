package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// A Reporter bot cannot resolve threads: it says the finding looks fixed,
// asks a human to close the thread, and does not repeat that next time.
func TestReporterBotSuggestsClosingThread(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.ForbidResolve = true
	oldID := finding.ID("cache.go", "Leaking goroutine")
	pos := &core.Position{NewPath: "cache.go", OldPath: "cache.go", NewLine: 2}
	h.GL.Discussions = []core.Discussion{
		{ID: "d-old", Resolvable: true, Notes: []core.Note{
			reviewtest.BotNote(2, finding.NoteBody(oldID, output.Finding{Severity: "minor", Title: "Leaking goroutine", Body: "never exits"}, false, 0), pos),
		}},
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, []output.ResolvedFinding{{ID: oldID, Comment: "теперь завершается"}})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	r := reviewtest.LastReply(t, h.GL)
	if r.DiscussionID != "d-old" || !strings.Contains(r.Body, "Похоже, исправлено: теперь завершается") ||
		!strings.Contains(r.Body, "Тред можно закрыть") || !strings.Contains(r.Body, marker.Fixed) {
		t.Fatalf("unexpected reply: %+v", r)
	}
	if strings.Contains(r.Body, "Не удалось") {
		t.Errorf("403 is expected for a Reporter, must not read as an error: %q", r.Body)
	}

	// Next review: the thread (still unresolved, now with the bot's fixed
	// reply) is not an open finding any more and gets no second reply.
	h.GL.Discussions[0].Notes = append(h.GL.Discussions[0].Notes, reviewtest.BotNote(3, r.Body, nil))
	replies := len(h.GL.Replies)
	h.GL.MR.DiffRefs.HeadSHA = "head2"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, []output.ResolvedFinding{{ID: oldID}})}}
	if err := h.R().Handle(context.Background(), core.Job{Kind: core.JobFullReview, ProjectID: 42, ProjectPath: "group/api", MRIID: 7, UserID: reviewtest.UserID, Username: "bob", DiscussionID: "cmd", NoteID: 99}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.Runner.Reqs); n != 2 {
		t.Fatalf("expected a second Claude run, got %d", n)
	}
	if strings.Contains(h.Runner.Reqs[1].Prompt, oldID) {
		t.Error("a thread the bot already declared fixed must not be an open finding again")
	}
	for _, rp := range h.GL.Replies[replies:] {
		if rp.DiscussionID == "d-old" {
			t.Errorf("second reply on the fixed thread: %+v", rp)
		}
	}
}

// Without a service token, a blocking thread the bot marked fixed stays
// unresolved in GitLab and still blocks: the gate line counts it, this run
// and the following ones, until a human closes it.
func TestReporterFixedBlockingStillCountsInGate(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.ForbidResolve = true
	critID := finding.ID("cache.go", "Data race")
	minorID := finding.ID("cache.go", "Leaking goroutine")
	openID := finding.ID("cache.go", "Nil deref")
	pos := &core.Position{NewPath: "cache.go", OldPath: "cache.go", NewLine: 2}
	h.GL.Discussions = []core.Discussion{
		{ID: "d-crit", Resolvable: true, Notes: []core.Note{
			reviewtest.BotNote(2, finding.NoteBodyOpts(critID, output.Finding{Severity: "critical", Title: "Data race", Body: "x"}, false, 0, finding.NoteOpts{Blocking: true}), pos)}},
		{ID: "d-minor", Resolvable: true, Notes: []core.Note{
			reviewtest.BotNote(3, finding.NoteBody(minorID, output.Finding{Severity: "minor", Title: "Leaking goroutine", Body: "y"}, false, 0), pos)}},
		{ID: "d-open", Resolvable: true, Notes: []core.Note{
			reviewtest.BotNote(4, finding.NoteBodyOpts(openID, output.Finding{Severity: "critical", Title: "Nil deref", Body: "z"}, false, 0, finding.NoteOpts{Blocking: true}), pos)}},
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, []output.ResolvedFinding{{ID: critID, Comment: "ok"}, {ID: minorID}})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	want := "⛔ **Мерж заблокирован:** 2 блокирующих замечания (из них 1 исправлено, ждёт закрытия)"
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, want) {
		t.Fatalf("gate line, want %q:\n%s", want, sum)
	}

	// Next review: the fixed-but-unresolved thread (now carrying the bot's
	// fixed reply) is still counted.
	for i := range h.GL.Discussions {
		for _, rp := range h.GL.Replies {
			if rp.DiscussionID == h.GL.Discussions[i].ID {
				h.GL.Discussions[i].Notes = append(h.GL.Discussions[i].Notes, reviewtest.BotNote(int64(100+i), rp.Body, nil))
			}
		}
	}
	h.GL.MR.DiffRefs.HeadSHA = "abcdef2222222222"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, []output.ResolvedFinding{{ID: openID}})}}
	if err := h.R().Handle(context.Background(), core.Job{Kind: core.JobFullReview, ProjectID: reviewtest.PID, ProjectPath: "group/api", MRIID: reviewtest.IID,
		UserID: reviewtest.UserID, Username: "bob", DiscussionID: "cmd", NoteID: 99}); err != nil {
		t.Fatal(err)
	}
	want = "⛔ **Мерж заблокирован:** 2 блокирующих замечания (из них 2 исправлены, ждут закрытия)"
	if sum := latestSummary(t, h); !strings.Contains(sum, want) {
		t.Fatalf("gate line on the next review, want %q:\n%s", want, sum)
	}

	// A human closes them: no longer counted.
	for i := range h.GL.Discussions {
		h.GL.Discussions[i].Resolved = true
	}
	h.GL.MR.DiffRefs.HeadSHA = "abcdef3333333333"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), core.Job{Kind: core.JobFullReview, ProjectID: reviewtest.PID, ProjectPath: "group/api", MRIID: reviewtest.IID,
		UserID: reviewtest.UserID, Username: "bob", DiscussionID: "cmd", NoteID: 100}); err != nil {
		t.Fatal(err)
	}
	if sum := latestSummary(t, h); !strings.Contains(sum, "✅ Блокирующих замечаний нет.") {
		t.Fatalf("resolved threads must not block:\n%s", sum)
	}
}

// latestSummary is the current body of the bot's summary note (the harness
// keeps it across runs; command replies are other notes).
func latestSummary(t *testing.T, h *reviewtest.Harness) string {
	t.Helper()
	for i := len(h.GL.CreatedNotes) - 1; i >= 0; i-- {
		if b := h.GL.Notes[h.GL.CreatedNotes[i]]; marker.IsSummaryNote(b) {
			return b
		}
	}
	t.Fatal("no summary note")
	return ""
}
