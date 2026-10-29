package pipeline_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// Without a summary note there is nothing to refresh: the chain ends even
// while the pipeline runs.
func TestRefreshStatusWithoutSummaryEndsMut(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Pipeline = &core.Pipeline{Status: "running", SHA: "head111111111111"}
	job := core.Job{Kind: core.JobRefreshStatus, ProjectID: reviewtest.PID, ProjectPath: reviewtest.AutoJob().ProjectPath, MRIID: reviewtest.IID}
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatalf("want the chain to end, got %v", err)
	}
	if _, ok := h.Internals().Pipeline.Refreshing.StartedAt(job.MRKey()); ok {
		t.Error("chain must end")
	}
	if len(h.GL.CreatedNotes) != 0 {
		t.Errorf("notes %v", h.GL.CreatedNotes)
	}
}

// Coverage: a report entry whose job is missing does not stop the others;
// only successful jobs (named exactly or as parallel instances) are used;
// deleted and ignored files are not changed lines.
func TestCoverageReportsAndChangedLinesMut(t *testing.T) {
	h := reviewtest.CoverageHarness(t)
	h.GL.RepoConfig = "coverage:\n  reports:\n    - job: unit\n      path: u.xml\n    - job: tests\n      path: build/coverage.xml\n"
	// A failed "tests" job listed first, with a report that would count.
	h.GL.Jobs = append([]core.CIJob{{ID: 9, Name: "tests", Status: "failed"}}, h.GL.Jobs...)
	h.GL.Artifacts["9:build/coverage.xml"] = []byte(reviewtest.CovXML)
	h.GL.Diffs = append(h.GL.Diffs,
		core.FileDiff{OldPath: "go.sum", NewPath: "go.sum", Diff: "@@ -0,0 +1,1 @@\n+x\n"},
		core.FileDiff{OldPath: "gone.go", NewPath: "gone.go", DeletedFile: true, Diff: "@@ -1,1 +0,0 @@\n-x\n"})
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	blk := marker.CoverageRe.FindString(reviewtest.SummaryBody(t, h))
	for _, want := range []string{"50% (1 из 2 строк)", "джоба `unit` не найдена", "Отчёт: [tests 1/2](https://gitlab.example.com/group/api/-/jobs/1), `tests 2/2` ·"} {
		if !strings.Contains(blk, want) {
			t.Errorf("block lacks %q:\n%s", want, blk)
		}
	}
	if strings.Contains(blk, "go.sum") || strings.Contains(blk, "gone.go") {
		t.Errorf("ignored or deleted file counted:\n%s", blk)
	}
	if got := pipeline.MatchJobs([]core.CIJob{{Name: "tests", Status: "failed"}, {Name: "tests", Status: "success"}, {Name: "tests 1/x", Status: "success"},
		{Name: "tests x/2", Status: "success"}, {Name: "tests 2/2", Status: "success"}}, "tests"); len(got) != 2 || got[1].Name != "tests 2/2" {
		t.Errorf("matchJobs %+v", got)
	}
}
