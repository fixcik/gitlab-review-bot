package review_test

import (
	"context"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// answerProbe is the queue's run state of one attempt; it records how many
// replies GitLab had accepted each time the job was marked answered.
type answerProbe struct {
	reviewtest.QueueRun
	gl *reviewtest.FakeGitLab
	mu sync.Mutex
	at []int
}

func (p *answerProbe) MarkAnswered() {
	p.gl.Mu.Lock()
	n := len(p.gl.Replies)
	p.gl.Mu.Unlock()
	p.mu.Lock()
	p.at = append(p.at, n)
	p.mu.Unlock()
}

func (p *answerProbe) marks() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.at...)
}

// A job is marked answered (a restart then drops it instead of replaying
// it) by its final reply only: "@bot check" written inside a finding
// thread first posts the per-finding "fixed" reply into that same thread,
// which must not count — a restart right after it would lose the check's
// own reply.
func TestCheckInFindingThreadAnsweredByFinalReplyOnly(t *testing.T) {
	h := chkHarness(t)
	reviewtest.WithModerator(h)
	h.GL.Discussions = h.GL.Discussions[:4]
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkDataLoss), Status: "fixed", Comment: "a"})}}
	job := checkJob("f1") // the command is a reply in the finding's own thread
	run := &answerProbe{QueueRun: reviewtest.QueueRun{ForJob: job}, gl: h.GL}
	if err := h.R().Handle(core.WithRunState(context.Background(), run), job); err != nil {
		t.Fatal(err)
	}
	got := repliesTo(h.GL, "f1")
	if len(got) != 2 {
		t.Fatalf("replies in f1 %q, want the fixed reply and the check's reply", got)
	}
	h.GL.Mu.Lock()
	total := len(h.GL.Replies)
	h.GL.Mu.Unlock()
	if m := run.marks(); len(m) != 1 || m[0] != total {
		t.Fatalf("marked answered after replies %v, want once, after the final one (%d)", m, total)
	}
}

// Every command's final reply (here a question's answer) marks it answered;
// an automatic review never is (nobody asked).
func TestFinalReplyMarksAnswered(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "Ответ."})}}
	job := reviewtest.ChatJob("почему?")
	run := &answerProbe{QueueRun: reviewtest.QueueRun{ForJob: job}, gl: h.GL}
	if err := h.R().Handle(core.WithRunState(context.Background(), run), job); err != nil {
		t.Fatal(err)
	}
	if m := run.marks(); len(m) != 1 || m[0] != 1 {
		t.Fatalf("marks %v", m)
	}

	// A reply for another job (not the running one) does not mark it.
	other := job
	other.NoteID++
	run2 := &answerProbe{QueueRun: reviewtest.QueueRun{ForJob: other}, gl: h.GL}
	if err := h.Env().Reply(core.WithRunState(context.Background(), run2), job, "x"); err != nil {
		t.Fatal(err)
	}
	if m := run2.marks(); len(m) != 0 {
		t.Fatalf("another job marked: %v", m)
	}
}
