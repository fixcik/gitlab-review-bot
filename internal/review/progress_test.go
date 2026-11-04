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
	reviewtest.WithFlags(h) // before the first R: pause is a flag of the store
	return h
}

func pauseMR(h *reviewtest.Harness) { h.Deps.MRFlags.(*reviewtest.FlagStore).Paused["42!7"] = "bob" }

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

// queued is the moment an automatic review is queued by an event: the
// thread opens in the background.
func queued(h *reviewtest.Harness, job core.Job) {
	h.R().Accepted(func(core.Job) {})(job)
	h.R().WaitProgress()
}

// closeByPerson resolves every discussion as a person.
func closeByPerson(h *reviewtest.Harness) {
	h.GL.Mu.Lock()
	defer h.GL.Mu.Unlock()
	for i := range h.GL.Discussions {
		h.GL.Discussions[i].Resolved = true
		h.GL.Discussions[i].Notes[0].ResolvedByID, h.GL.Discussions[i].Notes[0].ResolvedByUsername = 777, "alice"
	}
}

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
	queued(h, reviewtest.AutoJob())
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Discussions) != 0 || h.GL.CreatedThreads != 0 || len(h.GL.DeletedNotes) != 0 || len(h.GL.Replies) != 0 {
		t.Fatalf("thread without the option: %+v", h.GL.Discussions)
	}
}

func TestProgressThreadOpenedWhenQueued(t *testing.T) {
	h := progressHarness(t)
	queued(h, reviewtest.AutoJob())
	first := oneThread(t, h).Notes[0].Body
	if !strings.Contains(first, "Идёт ревью") || marker.HasBlocking(first) {
		t.Errorf("first note: %q", first)
	}
	// More events (a push, the other event source): still one thread.
	queued(h, reviewtest.AutoJob())
	queued(h, reviewtest.AutoJob())
	if n := len(progressThreads(h)); n != 1 || h.GL.CreatedThreads != 1 {
		t.Fatalf("threads %d, created %d", n, h.GL.CreatedThreads)
	}
	// Concurrent events too.
	h2 := progressHarness(t)
	for i := 0; i < 8; i++ {
		h2.R().Accepted(func(core.Job) {})(reviewtest.AutoJob())
	}
	h2.R().WaitProgress()
	if h2.GL.CreatedThreads != 1 {
		t.Fatalf("created %d threads", h2.GL.CreatedThreads)
	}
}

func TestProgressThreadNotOpenedWhenNoReviewIsExpected(t *testing.T) {
	for name, setup := range map[string]func(h *reviewtest.Harness){
		"draft":  func(h *reviewtest.Harness) { h.GL.MR.Draft = true },
		"closed": func(h *reviewtest.Harness) { h.GL.MR.State = "closed" },
		"head reviewed": func(h *reviewtest.Harness) {
			h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "head111111111111"}
		},
		"ignore marker": func(h *reviewtest.Harness) { h.GL.MR.Description = "@" + reviewtest.BotName + " ignore" },
		"paused":        pauseMR,
	} {
		t.Run(name, func(t *testing.T) {
			h := progressHarness(t)
			setup(h)
			queued(h, reviewtest.AutoJob())
			if h.GL.CreatedThreads != 0 {
				t.Fatalf("thread opened: %+v", h.GL.Discussions)
			}
		})
	}
	// Manual commands do not open it.
	h := progressHarness(t)
	queued(h, reviewtest.ManualJob(core.JobReview))
	if h.GL.CreatedThreads != 0 {
		t.Fatal("thread opened for a command")
	}
}

func TestProgressThreadDeletedAfterReview(t *testing.T) {
	h := progressHarness(t)
	queued(h, reviewtest.AutoJob())
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(progressThreads(h)); n != 0 {
		t.Fatalf("thread stays: %+v", h.GL.Discussions)
	}
	if len(h.GL.Replies) != 0 || len(h.GL.Resolved) != 0 {
		t.Fatalf("replies %v resolved %v", h.GL.Replies, h.GL.Resolved)
	}
}

func TestProgressThreadDeletedWhenNoReviewIsNeeded(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(h *reviewtest.Harness)
		job    core.Job
	}{
		"draft":         {func(h *reviewtest.Harness) { h.GL.MR.Draft = true }, reviewtest.AutoJob()},
		"closed":        {func(h *reviewtest.Harness) { h.GL.MR.State = "merged" }, reviewtest.AutoJob()},
		"pause":         {pauseMR, reviewtest.AutoJob()},
		"ignore marker": {func(h *reviewtest.Harness) { h.GL.MR.Description = "@" + reviewtest.BotName + " ignore" }, reviewtest.AutoJob()},
		"auto review off": {func(h *reviewtest.Harness) {
			h.GL.RepoConfig = "auto_review: false\n"
		}, reviewtest.AutoJob()},
		"head reviewed": {func(h *reviewtest.Harness) {
			h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "head111111111111"}
		}, reviewtest.AutoJob()},
		"nothing to review": {func(h *reviewtest.Harness) { h.Repos.Diff, h.Repos.Files = "", nil }, reviewtest.AutoJob()},
	} {
		t.Run(name, func(t *testing.T) {
			h := progressHarness(t)
			queued(h, reviewtest.AutoJob())
			if h.GL.CreatedThreads != 1 {
				t.Fatal("no thread opened")
			}
			tc.change(h)
			if err := handle(t, h, tc.job); err != nil {
				t.Fatal(err)
			}
			if n := len(progressThreads(h)); n != 0 {
				t.Fatalf("thread stays: %+v", h.GL.Discussions)
			}
		})
	}
}

func TestProgressThreadDeletedOnTrivialIncrement(t *testing.T) {
	h := trivialHarness(t, fileDiff("README.md", "@@ -1 +1,2 @@\n # API\n+More words.\n"), []string{"README.md"}, nil, nil)
	h.Cfg.Review.ProgressThread = true
	queued(h, reviewtest.AutoJob()) // reviewed at trivOld, the head moved on
	if h.GL.CreatedThreads != 1 {
		t.Fatal("no thread opened")
	}
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || len(progressThreads(h)) != 0 {
		t.Fatalf("claude %d, threads %d", len(h.Runner.Reqs), len(progressThreads(h)))
	}
}

func TestProgressThreadDeletedWhenTooBig(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Cfg.Review.ProgressThread = true
	queued(h.Harness, reviewtest.AutoJob())
	if h.GL.CreatedThreads != 1 {
		t.Fatal("no thread opened")
	}
	if err := handle(t, h.Harness, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || len(progressThreads(h.Harness)) != 0 {
		t.Fatalf("claude %d, threads %d", len(h.Runner.Reqs), len(progressThreads(h.Harness)))
	}
}

func TestProgressThreadDeletedWithItsReplies(t *testing.T) {
	h := progressHarness(t)
	queued(h, reviewtest.AutoJob())
	h.Runner.Resps = []reviewtest.RunResp{{Err: context.DeadlineExceeded}}
	_ = handle(t, h, reviewtest.AutoJob())
	if n := len(oneThread(t, h).Notes); n != 2 {
		t.Fatalf("notes: %d", n)
	}
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(progressThreads(h)) != 0 || len(h.GL.DeletedNotes) != 2 {
		t.Fatalf("threads %d, deleted %v", len(progressThreads(h)), h.GL.DeletedNotes)
	}
}

func TestProgressThreadStaysOpenOnError(t *testing.T) {
	h := progressHarness(t)
	queued(h, reviewtest.AutoJob())
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

func TestProgressThreadOpenedByFailureWhenMissing(t *testing.T) {
	h := progressHarness(t) // not queued by an event (a restored job): the reason opens it
	h.Runner.Resps = []reviewtest.RunResp{{Err: context.DeadlineExceeded}}
	_ = handle(t, h, reviewtest.AutoJob())
	if d := oneThread(t, h); d.Resolved || len(d.Notes) != 1 || !strings.Contains(d.Notes[0].Body, "⚠️ Ревью не удалось") {
		t.Fatalf("%+v", d)
	}
}

func TestProgressThreadStaysOpenWhenPostponed(t *testing.T) {
	h := progressHarness(t)
	queued(h, reviewtest.AutoJob())
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
	// The review passes at last: the thread is gone.
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(progressThreads(h)) != 0 {
		t.Fatal("thread stays after the review")
	}
}

func TestProgressThreadNewCommitKeepsOneThread(t *testing.T) {
	h := progressHarness(t)
	queued(h, reviewtest.AutoJob())
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	// A second commit: a new thread, deleted again after the review.
	h.GL.MR.DiffRefs.HeadSHA = "head222222222222"
	job := reviewtest.AutoJob()
	job.HeadSHA = "head222222222222"
	queued(h, job)
	if d := oneThread(t, h); len(d.Notes) != 1 || !strings.Contains(d.Notes[0].Body, "Идёт ревью") {
		t.Fatalf("%+v", d)
	}
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, job); err != nil {
		t.Fatal(err)
	}
	if len(progressThreads(h)) != 0 {
		t.Fatal("thread stays")
	}
}

// A push during a running review: the thread is the next review's too.
func TestProgressThreadKeptForNewerQueuedCommit(t *testing.T) {
	h := progressHarness(t)
	queued(h, reviewtest.AutoJob())
	newer := reviewtest.AutoJob()
	newer.HeadSHA = "head222222222222"
	queued(h, newer)
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(progressThreads(h)) != 1 {
		t.Fatal("thread deleted while a newer commit waits")
	}
	h.GL.MR.DiffRefs.HeadSHA = "head222222222222"
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, newer); err != nil {
		t.Fatal(err)
	}
	if len(progressThreads(h)) != 0 {
		t.Fatal("thread stays after the last review")
	}
}

func TestProgressThreadClosedByPersonIsLeftAlone(t *testing.T) {
	h := progressHarness(t)
	queued(h, reviewtest.AutoJob())
	closeByPerson(h)
	// Neither a new event, nor a failure, nor a success touches it.
	queued(h, reviewtest.AutoJob())
	h.Runner.Resps = []reviewtest.RunResp{{Err: context.DeadlineExceeded}}
	_ = handle(t, h, reviewtest.AutoJob())
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if d := oneThread(t, h); !d.Resolved || len(d.Notes) != 1 || h.GL.CreatedThreads != 1 || len(h.GL.DeletedNotes) != 0 {
		t.Fatalf("the person's override was touched: %+v created %d", d, h.GL.CreatedThreads)
	}
}

func TestProgressThreadOpenOnConflict(t *testing.T) {
	restore := review.SetMergeStatusWaits(0, 0)
	t.Cleanup(restore)
	h := progressHarness(t)
	queued(h, reviewtest.AutoJob())
	h.GL.MR.TargetBranch = "dev"
	h.GL.MR.MergeStatus, h.GL.MR.HasConflicts = "conflict", true
	for i := 0; i < 2; i++ {
		if err := handle(t, h, reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
	}
	d := oneThread(t, h)
	if d.Resolved || !strings.Contains(lastBody(d), "⛔ Ревью не запускалось: конфликт слияния с `dev`") || len(d.Notes) != 2 {
		t.Fatalf("resolved=%v notes=%+v", d.Resolved, d.Notes)
	}
	// The conflict is resolved and the review runs: the thread is gone.
	h.GL.MR.MergeStatus, h.GL.MR.HasConflicts = "can_be_merged", false
	h.Runner.Resps = okRun(t)
	if err := handle(t, h, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(progressThreads(h)) != 0 {
		t.Fatal("thread stays after the review")
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
