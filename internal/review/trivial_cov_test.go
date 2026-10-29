package review_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// A whitespace-only increment of cache.go (trivialHarness).
var (
	trivWSDiff   = fileDiff("cache.go", "@@ -1 +1 @@\n-x := 1\n+x  :=  1\n")
	trivWSBefore = map[string]string{"cache.go": "x := 1\n"}
	trivWSAfter  = map[string]string{"cache.go": "x  :=  1\n"}
)

func trivialWS(t *testing.T) *reviewtest.Harness {
	t.Helper()
	return trivialHarness(t, trivWSDiff, []string{"cache.go"}, trivWSBefore, trivWSAfter)
}

// Increments the comparison does not look into are reviewed by Claude.
func TestTrivialIncrementNotClassified(t *testing.T) {
	var many strings.Builder
	var manyFiles []string
	before, after := map[string]string{}, map[string]string{}
	for i := 0; i < 51; i++ {
		p := fmt.Sprintf("f%02d.go", i)
		many.WriteString(fileDiff(p, "@@ -1 +1 @@\n-x := 1\n+x  :=  1\n"))
		manyFiles = append(manyFiles, p)
		before[p], after[p] = "x := 1\n", "x  :=  1\n"
	}
	for _, tc := range []struct {
		name          string
		diff          string
		files         []string
		before, after map[string]string
	}{
		{"more than 50 files", many.String(), manyFiles, before, after},
		{"diff without a path", "@@ -1 +1 @@\n-x := 1\n+x  :=  1\n", []string{"cache.go"}, trivWSBefore, trivWSAfter},
		{"new version unreadable", trivWSDiff, []string{"cache.go"}, trivWSBefore, nil},
		{"old version unreadable", trivWSDiff, []string{"cache.go"}, nil, trivWSAfter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := trivialHarness(t, tc.diff, tc.files, tc.before, tc.after)
			var skips []string
			h.Deps.OnSkip = func(r string) { skips = append(skips, r) }
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if len(h.Runner.Reqs) != 1 || len(skips) != 0 {
				t.Fatalf("claude runs %d skips %v", len(h.Runner.Reqs), skips)
			}
		})
	}
}

// The thread scan of a trivial increment fails: the job fails and the
// reviewed head stays (the next attempt classifies the increment again).
func TestTrivialIncrementScanError(t *testing.T) {
	h := trivialWS(t)
	gl, _, _ := reviewtest.WithFaults(h)
	gl.Fail = reviewtest.FailOn(errBoom, "ListDiscussions")
	err := h.R().Handle(context.Background(), reviewtest.AutoJob())
	if !errors.Is(err, errBoom) {
		t.Fatalf("err %v", err)
	}
	if st, _ := h.St.Get(); st.ReviewedSHA != trivOld {
		t.Fatalf("state %+v", st)
	}
	if len(h.Runner.Reqs) != 0 || len(h.GL.UpdatedNotes) != 0 {
		t.Fatalf("runs %d updated %v", len(h.Runner.Reqs), h.GL.UpdatedNotes)
	}
}

// trivialTodo: a trivial increment that adds a TODO comment (the todo
// check publishes a thread, so the gate recount reads the threads anew).
func trivialTodo(t *testing.T) *reviewtest.Harness {
	t.Helper()
	diff := fileDiff("cache.go", "@@ -1,2 +1,3 @@\n x := 1\n+// TODO: drop this\n y := 2\n")
	h := trivialHarness(t, diff, []string{"cache.go"}, map[string]string{"cache.go": "x := 1\ny := 2\n"},
		map[string]string{"cache.go": "x := 1\n// TODO: drop this\ny := 2\n"})
	h.Idx.Lines["cache.go"] = map[int]int{1: 0, 2: 1, 3: 0}
	h.GL.Discussions = append(h.GL.Discussions, sizeThreadDisc("size", 600)) // turns the gate red
	return h
}

// The gate recount cannot rescan the threads: the summary keeps its gate
// line, the rest of the trivial path goes on.
func TestTrivialIncrementGateRecountError(t *testing.T) {
	h := trivialTodo(t)
	gl, _, _ := reviewtest.WithFaults(h)
	// The first real read is the config note's; the trivial path's scan is
	// answered from the job's memo, the recount after the todo thread was
	// published reads again, as does the bot resolving the non-blocking todo
	// thread with its token: every read after the first fails.
	gl.Fail = func(m string, n int) error {
		if m == "ListDiscussions" && n >= 2 {
			return errBoom
		}
		return nil
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if gl.Calls("ListDiscussions") != 3 {
		t.Fatalf("reads %d", gl.Calls("ListDiscussions"))
	}
	body := h.GL.Notes[500]
	if !strings.Contains(body, summary.GateClearLine) || !strings.Contains(body, summary.TrivialStatusPrefix) {
		t.Fatalf("summary:\n%s", body)
	}
	if st, _ := h.St.Get(); st.ReviewedSHA != trivHead {
		t.Fatalf("state %+v", st)
	}

	// Without the failure the same increment recounts the gate.
	h2 := trivialTodo(t)
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if body := h2.GL.Notes[500]; !strings.Contains(body, summary.GateLine(1, 0)) {
		t.Fatalf("gate not recounted:\n%s", body)
	}
}

// The summary cannot be written: the reviewed head still advances and the
// state keeps pointing at the summary note it knows.
func TestTrivialIncrementSummaryWriteError(t *testing.T) {
	t.Run("update fails", func(t *testing.T) {
		h := trivialWS(t)
		gl, _, _ := reviewtest.WithFaults(h)
		gl.Fail = reviewtest.FailOn(errBoom, "UpdateNote")
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		if st, _ := h.St.Get(); st.ReviewedSHA != trivHead || st.SummaryNoteID != 500 {
			t.Fatalf("state %+v", st)
		}
	})
	t.Run("summary gone, create fails", func(t *testing.T) {
		h := trivialWS(t)
		h.GL.Discussions = nil // the summary note was deleted
		gl, _, _ := reviewtest.WithFaults(h)
		gl.Fail = reviewtest.FailOn(errBoom, "CreateNote")
		if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		if st, _ := h.St.Get(); st.ReviewedSHA != trivHead || st.SummaryNoteID != 500 {
			t.Fatalf("state %+v", st)
		}
		if gl.Calls("CreateNote") != 1 {
			t.Fatalf("create calls %d", gl.Calls("CreateNote"))
		}
	})
}

// The trunk-based block of the summary is replaced with the new head's.
func TestTrivialIncrementRefreshesTBD(t *testing.T) {
	h := trivialWS(t)
	old := marker.TBDStart + "\nстарый статус\n" + marker.TBDEnd
	h.GL.Discussions[0].Notes[0].Body = strings.Replace(h.GL.Discussions[0].Notes[0].Body, "Старое саммари.", old+"\n\nСтарое саммари.", 1)
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	body := h.GL.Notes[500]
	if strings.Contains(body, "старый статус") || strings.Count(body, marker.TBDStart) != 1 || !strings.Contains(body, "Старое саммари.") {
		t.Fatalf("summary:\n%s", body)
	}
}

// With lint off only the todo check runs (no linter is asked).
func TestTrivialIncrementLintOff(t *testing.T) {
	fl := &reviewtest.FakeLint{}
	h2 := trivialWS(t)
	h2.Deps.Lint = fl
	h2.Cfg.Review.Lint.Enabled = false
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(fl.Calls) != 0 || len(h2.Runner.Reqs) != 0 {
		t.Fatalf("lint calls %d runs %d", len(fl.Calls), len(h2.Runner.Reqs))
	}
	if st, _ := h2.St.Get(); st.ReviewedSHA != trivHead {
		t.Fatalf("state %+v", st)
	}

	// Lint on without gitleaks: no tool of the linter runs on the increment.
	h3 := trivialWS(t)
	fl3 := &reviewtest.FakeLint{Res: &lint.Result{}}
	h3.Deps.Lint = fl3
	h3.Cfg.Review.Lint.Enabled = true
	h3.Cfg.Review.Lint.Tools = []string{"golangci-lint"}
	if err := h3.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	for _, c := range fl3.Calls {
		if len(c.Config.Tools) != 0 {
			t.Fatalf("tools %v on a trivial increment", c.Config.Tools)
		}
	}
}

// What the trivial run could not check goes into the summary's problems
// block: added under the status line when the summary has none yet,
// replaced in place when it has one.
func TestTrivialIncrementProblemsBlock(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing block %v", existing), func(t *testing.T) {
			ph := reviewtest.NewProbHarness(t, "")
			h := ph.Harness
			setupTrivial(h, trivWSDiff, []string{"cache.go"}, trivWSBefore, trivWSAfter)
			if existing {
				h.GL.Discussions[0].Notes[0].Body += "\n" + marker.ProblemsStart + "\nстарые проблемы\n" + marker.ProblemsEnd + "\n"
			}
			h.Cfg.Review.Lint.Enabled = true
			h.Deps.Lint = &reviewtest.FakeLint{Res: &lint.Result{Tools: []lint.ToolStatus{
				{Tool: lint.ToolGitleaks, State: lint.StateFailed, Reason: "панический выход"}}}}
			if err := ph.Svc.Handler(h.R().Handle)(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			body := h.GL.Notes[500]
			block := reviewtest.ProblemsBlockOf(body)
			if !strings.Contains(block, "gitleaks") || strings.Contains(body, "старые проблемы") || strings.Count(body, marker.ProblemsStart) != 1 {
				t.Fatalf("summary:\n%s", body)
			}
			if len(h.Runner.Reqs) != 0 {
				t.Fatal("claude ran")
			}
		})
	}
}

// setupTrivial is trivialHarness on an existing harness.
func setupTrivial(h *reviewtest.Harness, diff string, files []string, before, after map[string]string) {
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: trivOld, SummaryNoteID: 500}
	h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(500,
		marker.Summary(trivOld)+"\n"+summary.Title+"\n\n"+summary.GateClearLine+"\n\nСтарое саммари.\n", nil)}}}
	h.Repos.Ancestors[trivOld] = true
	h.Repos.Diff, h.Repos.Files = diff, files
	h.Repos.ShowAt = map[string]string{}
	h.Repos.FilesAt = map[string][]string{trivOld: files, trivHead: files}
	for p, s := range before {
		h.Repos.ShowAt[trivOld+":"+p] = s
	}
	for p, s := range after {
		h.Repos.ShowAt[trivHead+":"+p] = s
	}
}
