package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// A person closes a blocking lint thread in the GitLab UI (no event) and
// replied in it: the reply refreshes the merge-gate line of the summary.
func TestThreadReplyInResolvedBotThreadRefreshesGate(t *testing.T) {
	run := func(t *testing.T, resolved bool, author string) string {
		h := reviewtest.New(t)
		gate := "⛔ **Мерж заблокирован:** 1 блокирующее замечание. Исправьте и закройте треды."
		h.GL.Discussions = []core.Discussion{
			{ID: "sum", Notes: []core.Note{reviewtest.BotNote(500, marker.Summary("abc123")+"\n"+summary.Title+"\n\n"+gate+"\n\nТекст.\n", nil)}},
			{ID: "l1", Resolvable: true, Resolved: resolved, Notes: []core.Note{
				reviewtest.BotNote(3, marker.Lint("ccc", "gitleaks")+"\n"+marker.Blocking+"\nsecret", nil),
				{ID: 6, AuthorID: 7, AuthorUsername: author, Body: "это не секрет"},
			}},
		}
		h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "abc123", SummaryNoteID: 500}
		job := reviewtest.ReplyJob(6)
		job.DiscussionID = "l1"
		if err := h.R().Handle(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		return h.GL.Notes[500]
	}
	if body := run(t, true, "alice"); !strings.Contains(body, "\n"+summary.GateClearLine+"\n") || strings.Contains(body, "Мерж заблокирован") {
		t.Fatalf("the gate must clear:\n%s", body)
	}
	if body := run(t, false, "alice"); body != "" {
		t.Fatalf("an open thread must not touch the summary:\n%s", body)
	}
}
