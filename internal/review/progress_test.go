package review_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func progressHarness(t *testing.T) *reviewtest.Harness {
	t.Helper()
	h := reviewtest.New(t)
	h.Cfg.Review.ProgressThread = true
	h.GL.PublishMakesThreads = true
	return h
}

func okRun(t *testing.T) []reviewtest.RunResp {
	return []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
}

// progressThreads returns the discussions headed by the progress marker.
func progressThreads(h *reviewtest.Harness) []core.Discussion {
	h.GL.Mu.Lock()
	defer h.GL.Mu.Unlock()
	var out []core.Discussion
	for _, d := range h.GL.Discussions {
		if len(d.Notes) > 0 && strings.Contains(d.Notes[0].Body, marker.ProgressThread) {
			out = append(out, d)
		}
	}
	return out
}

func lastBody(d core.Discussion) string { return d.Notes[len(d.Notes)-1].Body }

func oneThread(t *testing.T, h *reviewtest.Harness) core.Discussion {
	t.Helper()
	ds := progressThreads(h)
	if len(ds) != 1 {
		t.Fatalf("progress threads: %d", len(ds))
	}
	return ds[0]
}

func handle(t *testing.T, h *reviewtest.Harness, job core.Job) error {
	t.Helper()
	return h.R().Handle(context.Background(), job)
}

func TestProgressThreadOffByDefault(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.PublishMakesThreads = true
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(progressThreads(h)) != 0 || len(h.GL.Replies) != 0 {
		t.Fatalf("thread without the option: %+v %+v", h.GL.Discussions, h.GL.Replies)
	}
}

func TestProgressThreadClosedAfterReview(t *testing.T) {
	h := progressHarness(t)
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	d := oneThread(t, h)
	first := d.Notes[0].Body
	if !strings.Contains(first, "Идёт ревью") || marker.HasBlocking(first) {
		t.Errorf("first note: %q", first)
	}
	if !d.Resolved || !strings.Contains(lastBody(d), "✅ Ревью завершено (`head1111`)") {
		t.Errorf("not closed with the done reply: resolved=%v last=%q", d.Resolved, lastBody(d))
	}
}

func TestProgressThreadNotForSkippedReviews(t *testing.T) {
	h := progressHarness(t)
	h.GL.MR.Draft = true
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	h.GL.MR.Draft = false
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	n := len(h.GL.Replies)
	// The head is reviewed now: nothing is opened, replied or reopened.
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Replies) != n || len(progressThreads(h)) != 1 {
		t.Fatalf("a reviewed head touched the thread: replies %d -> %d", n, len(h.GL.Replies))
	}
}

func TestProgressThreadStaysOpenOnError(t *testing.T) {
	h := progressHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Err: context.DeadlineExceeded}}
	if err := handle(t, h, reviewtest.AutoJob()); err == nil {
		t.Fatal("want an error")
	}
	d := oneThread(t, h)
	last := lastBody(d)
	if d.Resolved || !strings.Contains(last, "⚠️ Ревью не удалось: "+review.ReasonInternal) || !strings.Contains(last, "@"+reviewtest.BotName+" review") {
		t.Fatalf("resolved=%v last=%q", d.Resolved, last)
	}
	// The job's retry fails the same way: no second reply.
	h.Runner.Resps = []reviewtest.RunResp{{Err: context.DeadlineExceeded}}
	_ = handle(t, h, reviewtest.AutoJob())
	if n := len(oneThread(t, h).Notes); n != 2 {
		t.Fatalf("notes after the retry: %d", n)
	}
}

func TestProgressThreadStaysOpenWhenPostponed(t *testing.T) {
	h := progressHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Err: &core.ErrClaudeRateLimited{ResetAt: reviewtest.TestNow.Add(time.Hour)}}}
	err := handle(t, h, reviewtest.AutoJob())
	if _, ok := core.AsRetryLater(err); !ok {
		t.Fatalf("err = %v", err)
	}
	d := oneThread(t, h)
	if d.Resolved || !strings.Contains(lastBody(d), "⏳ Ревью отложено до") || !strings.Contains(lastBody(d), review.ReasonClaudeLimit) {
		t.Fatalf("resolved=%v last=%q", d.Resolved, lastBody(d))
	}
	// The retry postpones again, later: the same state, no new reply.
	h.Runner.Resps = []reviewtest.RunResp{{Err: &core.ErrClaudeRateLimited{ResetAt: reviewtest.TestNow.Add(2 * time.Hour)}}}
	_ = handle(t, h, reviewtest.AutoJob())
	if n := len(oneThread(t, h).Notes); n != 2 {
		t.Fatalf("notes after the retry: %d", n)
	}
}

func TestProgressThreadReopenedByNewCommit(t *testing.T) {
	h := progressHarness(t)
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	h.GL.MR.DiffRefs.HeadSHA = "head222222222222"
	h.Runner.Resps = []reviewtest.RunResp{{Err: context.DeadlineExceeded}}
	_ = handle(t, h, reviewtest.AutoJob())
	d := oneThread(t, h) // the same thread, not a second one
	if d.Resolved {
		t.Fatal("the thread stayed closed")
	}
	var got []string
	for _, n := range d.Notes {
		got = append(got, n.Body)
	}
	if all := strings.Join(got, "\n"); !strings.Contains(all, "⏳ Ревью нового коммита `head2222`…") {
		t.Fatalf("notes: %q", all)
	}
	// The thread is open and shows a failure: a retry of the same commit adds nothing.
	n := len(d.Notes)
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	d = oneThread(t, h)
	if !d.Resolved || len(d.Notes) != n+1 || !strings.Contains(lastBody(d), "✅ Ревью завершено (`head2222`)") {
		t.Fatalf("resolved=%v notes %d -> %d", d.Resolved, n, len(d.Notes))
	}
}

func TestProgressThreadClosedByPersonIsNotReopened(t *testing.T) {
	h := progressHarness(t)
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	h.GL.Mu.Lock()
	for i := range h.GL.Discussions {
		h.GL.Discussions[i].Resolved = true
		h.GL.Discussions[i].Notes[0].ResolvedByID, h.GL.Discussions[i].Notes[0].ResolvedByUsername = 777, "alice"
	}
	h.GL.Mu.Unlock()
	n := len(oneThread(t, h).Notes)
	h.GL.MR.DiffRefs.HeadSHA = "head222222222222"
	h.Runner.Resps = []reviewtest.RunResp{{Err: context.DeadlineExceeded}}
	_ = handle(t, h, reviewtest.AutoJob())
	if d := oneThread(t, h); !d.Resolved || len(d.Notes) != n {
		t.Fatalf("the person's override was touched: resolved=%v notes %d -> %d", d.Resolved, n, len(d.Notes))
	}
}

func TestProgressThreadOpenOnConflict(t *testing.T) {
	restore := review.SetMergeStatusWaits(0, 0)
	t.Cleanup(restore)
	h := progressHarness(t)
	h.GL.MR.TargetBranch = "dev"
	h.GL.MR.MergeStatus, h.GL.MR.HasConflicts = "conflict", true
	for i := 0; i < 2; i++ {
		if err := handle(t, h, reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
	}
	d := oneThread(t, h)
	if d.Resolved || !strings.Contains(d.Notes[0].Body, "⛔ Ревью не запускалось: конфликт слияния с `dev`") || len(d.Notes) != 1 {
		t.Fatalf("resolved=%v notes=%+v", d.Resolved, d.Notes)
	}
}

func TestProgressThreadOpenWhenCommandDenied(t *testing.T) {
	h := progressHarness(t)
	h.Lim.User = core.Decision{Reason: "Лимит запусков исчерпан."}
	if err := handle(t, h, reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	d := oneThread(t, h)
	if d.Resolved || !strings.Contains(lastBody(d), "⛔ Ревью не запущено: Лимит запусков исчерпан.") {
		t.Fatalf("resolved=%v last=%q", d.Resolved, lastBody(d))
	}
}
