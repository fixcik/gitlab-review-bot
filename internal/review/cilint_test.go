package review_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// A job still running: the rest of the review is published now with a
// "⏳ ждём job" line; when the pipeline finishes the refresh chain reads the
// report and publishes the threads without a new Claude run.
func TestCILintPendingThenRefreshWithoutClaude(t *testing.T) {
	h, _, _ := reviewtest.CIHarness(t)
	now := reviewtest.TestNow
	h.Env().Now = func() time.Time { return now }
	var queued []core.Job
	h.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	h.Cfg.Review.TBD.Enabled = false // only the CI linters keep the chain going
	h.GL.MR.Pipeline.Status = "running"
	h.GL.Jobs[0].Status = "running"
	h.GL.Jobs[1].Status = "pending"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if n := len(reviewtest.LintThreads(h)); n != 0 {
		t.Fatalf("nothing to publish yet: %d", n)
	}
	if len(queued) != 1 || queued[0].Kind != core.JobRefreshStatus {
		t.Fatalf("refresh chain: %+v", queued)
	}
	p := h.Runner.Reqs[0].Prompt
	if !strings.Contains(p, "The CI jobs of these linters have not finished yet: ci:semgrep, ci:ruff.") {
		t.Errorf("prompt:\n%s", p)
	}
	sid := h.GL.CreatedNotes[0]
	sum := h.GL.Notes[sid]
	if !strings.Contains(sum, "**🔧 Линтеры из CI:** ⏳ ждём job `semgrep`, `ruff 1/2`") || !strings.Contains(sum, "<!-- reviewbot:cilint-pending ci:ruff,ci:semgrep -->") ||
		strings.Contains(marker.CILintRe.FindString(sum), "<details>") {
		t.Fatalf("pending block:\n%s", sum)
	}
	sync := func() { // the fake lists the summary note with its current body next to the threads
		h.GL.Mu.Lock()
		defer h.GL.Mu.Unlock()
		var ds []core.Discussion
		for _, d := range h.GL.Discussions {
			if d.ID != "s" {
				ds = append(ds, d)
			}
		}
		h.GL.Discussions = append(ds, core.Discussion{ID: "s", Notes: []core.Note{reviewtest.BotNote(sid, h.GL.Notes[sid], nil)}})
	}

	// Still running: retry later, nothing published.
	job := queued[0]
	sync()
	if _, ok := core.AsRetryLater(h.R().Handle(context.Background(), job)); !ok {
		t.Fatal("want RetryLater while the jobs run")
	}
	// semgrep finished; ruff still pending: semgrep's thread now.
	h.GL.Mu.Lock()
	h.GL.Jobs[0].Status = "success"
	h.GL.Mu.Unlock()
	sync()
	if _, ok := core.AsRetryLater(h.R().Handle(context.Background(), job)); !ok {
		t.Fatal("ruff still runs: want RetryLater")
	}
	if th := reviewtest.LintThreads(h); len(th) != 1 || !strings.Contains(th[0].Body, "ci:semgrep") {
		t.Fatalf("semgrep thread after its job: %+v", th)
	}
	// Everything finished.
	h.GL.Mu.Lock()
	h.GL.Jobs[1].Status = "failed"
	h.GL.MR.Pipeline.Status = "failed"
	h.GL.Mu.Unlock()
	now = now.Add(4 * time.Minute)
	sync()
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatalf("finished: %v", err)
	}
	if th := reviewtest.LintThreads(h); len(th) != 2 {
		t.Fatalf("threads after the pipeline: %+v", th)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("no new Claude run: %d", len(h.Runner.Reqs))
	}
	sum = h.GL.Notes[sid]
	blk := marker.CILintRe.FindString(sum)
	if strings.Contains(blk, "⏳") || !strings.Contains(blk, "<summary>🔧 Линтеры из CI: 2 замечания на изменённых строках</summary>") ||
		!strings.Contains(blk, "<!-- reviewbot:cilint-pending  -->") || strings.Count(sum, marker.CILintStart) != 1 {
		t.Errorf("block after the pipeline:\n%s", sum)
	}
	if _, ok := h.Internals().Pipeline.Refreshing.StartedAt(job.MRKey()); ok {
		t.Error("chain must end")
	}
	// A later refresh (another chain) with nothing pending does nothing.
	before := len(h.GL.Published)
	sync()
	if err := h.R().Handle(context.Background(), job); err != nil || len(h.GL.Published) != before {
		t.Errorf("nothing pending: %v %d", err, len(h.GL.Published)-before)
	}
}
