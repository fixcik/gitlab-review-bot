package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// The merge gate fails closed: a blocking result (finding, breaking change,
// secret) whose thread could not be published still blocks the merge, the
// summary says so and the reviewed head is not advanced, so the next review
// of the same range raises it again.

const oldHead = "old000000000000"

// incremental: the MR was reviewed up to oldHead before.
func incremental(h *reviewtest.Harness) {
	h.St.States[core.MRKey(reviewtest.PID, reviewtest.IID)] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: oldHead}
	h.Repos.Ancestors[oldHead] = true
}

func reviewedSHA(t *testing.T, h *reviewtest.Harness) string {
	t.Helper()
	st, ok := h.St.Get()
	if !ok {
		t.Fatal("no state")
	}
	return st.ReviewedSHA
}

var brokenCrit = output.Finding{Path: "cache.go", Line: 2, Severity: "critical", Title: "Broken note", Body: "плохо"}

func TestUnpublishedBlockingFindingKeepsGateBlocked(t *testing.T) {
	h := reviewtest.New(t)
	incremental(h)
	h.GL.FailDraftFor = "Broken"
	good := output.Finding{Path: "cache.go", Line: 3, Severity: "minor", Title: "Good note"}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{brokenCrit, good}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sum := reviewtest.SummaryBody(t, h)
	for _, s := range []string{summary.GateLine(1, 0), review.UnpublishedLine(1, "@reviewbot"), "Broken note"} {
		if !strings.Contains(sum, s) {
			t.Errorf("summary lacks %q:\n%s", s, sum)
		}
	}
	if got := reviewedSHA(t, h); got != oldHead {
		t.Fatalf("reviewed head advanced to %q", got)
	}

	// The next review (here a command on the same head) covers the same
	// range again and publishes the finding.
	h.GL.FailDraftFor = ""
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{brokenCrit}, nil)}}
	diffs := len(h.Repos.DiffFrom)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("runs %d", len(h.Runner.Reqs))
	}
	if from := h.Repos.DiffFrom[diffs]; from != oldHead {
		t.Fatalf("second range from %q", from)
	}
	if n := len(h.GL.Published); n != 2 || !strings.Contains(h.GL.Published[1].Body, "Broken note") {
		t.Fatalf("published %+v", h.GL.Published)
	}
	if got := reviewedSHA(t, h); got != "head111111111111" {
		t.Fatalf("reviewed head %q", got)
	}
}

// A non-blocking finding that could not be published is only listed: it
// neither blocks nor holds the reviewed head.
func TestUnpublishedNonBlockingFindingDoesNotBlock(t *testing.T) {
	h := reviewtest.New(t)
	incremental(h)
	h.GL.FailDraftFor = "Broken"
	minor := brokenCrit
	minor.Severity = "minor"
	minor.Line = 0 // not anchored either way
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{minor}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, summary.GateClearLine) || strings.Contains(sum, "не удалось опубликовать") {
		t.Fatalf("summary:\n%s", sum)
	}
	if got := reviewedSHA(t, h); got != "head111111111111" {
		t.Fatalf("reviewed head %q", got)
	}
}

// PublishDraftNotes fails: nothing of the batch is published, every
// blocking result in it counts.
func TestPublishFailureKeepsGateBlocked(t *testing.T) {
	h := reviewtest.New(t)
	incremental(h)
	gl, _, _ := reviewtest.WithFaults(h)
	gl.Fail = reviewtest.FailOn(errBoom, "PublishDraftNotes")
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{brokenCrit,
		{Path: "cache.go", Line: 3, Severity: "critical", Title: "Second"}}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, summary.GateLine(2, 0)) || !strings.Contains(sum, review.UnpublishedLine(2, "@reviewbot")) {
		t.Fatalf("summary:\n%s", sum)
	}
	if got := reviewedSHA(t, h); got != oldHead {
		t.Fatalf("reviewed head %q", got)
	}
}

func TestUnpublishedBreakingChangeKeepsGateBlocked(t *testing.T) {
	h := reviewtest.New(t)
	incremental(h)
	h.GL.FailDraftFor = "removed the optional property 'email'"
	h.Compat.Res = &compat.Result{Changes: []compat.Change{reviewtest.OasdiffChange}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, summary.GateLine(1, 0)) || !strings.Contains(sum, review.UnpublishedLine(1, "@reviewbot")) {
		t.Fatalf("summary:\n%s", sum)
	}
	if got := reviewedSHA(t, h); got != oldHead {
		t.Fatalf("reviewed head %q", got)
	}
}

func TestUnpublishedSecretKeepsGateBlocked(t *testing.T) {
	h, fl := reviewtest.LintHarness(t)
	incremental(h)
	fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.SecretFinding}, Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h.GL.FailDraftFor = "aws-access-token"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, summary.GateLine(1, 0)) || !strings.Contains(sum, review.UnpublishedLine(1, "@reviewbot")) {
		t.Fatalf("summary:\n%s", sum)
	}
	if got := reviewedSHA(t, h); got != oldHead {
		t.Fatalf("reviewed head %q", got)
	}
}

// The model's "resolved" list closes only findings in files of the reviewed
// range (old paths of renames included); others stay open and keep
// blocking.
func TestResolvedOnlyInReviewedFiles(t *testing.T) {
	h := reviewtest.New(t)
	h.Repos.Files = []string{"cache.go", "new.go"}
	h.Repos.Diff += "diff --git a/old.go b/new.go\nsimilarity index 100%\nrename from old.go\nrename to new.go\n"
	in := finding.ID("cache.go", "In range")
	out := finding.ID("other.go", "Out of range")
	gen := finding.ID("cache.go", "General in range")
	genOut := finding.ID("other.go", "General out of range")
	moved := finding.ID("old.go", "Renamed file")
	blocking := finding.NoteOpts{Blocking: true}
	general := finding.NoteOpts{Blocking: true, OutsideDiff: true}
	at := func(p string) *core.Position { return &core.Position{NewPath: p, OldPath: p, NewLine: 2} }
	h.GL.Discussions = []core.Discussion{
		{ID: "d-in", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1, finding.NoteBodyOpts(in, output.Finding{Path: "cache.go", Line: 2, Severity: "critical", Title: "In range"}, false, 0, blocking), at("cache.go"))}},
		{ID: "d-out", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(2, finding.NoteBodyOpts(out, output.Finding{Path: "other.go", Line: 2, Severity: "critical", Title: "Out of range"}, false, 0, blocking), at("other.go"))}},
		{ID: "d-gen", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(3, finding.NoteBodyOpts(gen, output.Finding{Path: "cache.go", Line: 40, Severity: "critical", Title: "General in range"}, false, 0, general), nil)}},
		{ID: "d-gen-out", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(4, finding.NoteBodyOpts(genOut, output.Finding{Path: "other.go", Line: 40, Severity: "critical", Title: "General out of range", Body: "_Файл `cache.go` (вне диффа)._"}, false, 0, general), nil)}},
		{ID: "d-moved", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(5, finding.NoteBodyOpts(moved, output.Finding{Path: "old.go", Line: 2, Severity: "critical", Title: "Renamed file"}, false, 0, blocking), at("old.go"))}},
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, []output.ResolvedFinding{{ID: in}, {ID: out}, {ID: gen}, {ID: genOut}, {ID: moved}})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.GL.Resolved, ","); got != "d-in,d-gen,d-moved" {
		t.Fatalf("resolved %s", got)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, summary.GateLine(2, 0)) || !strings.Contains(sum, "закрыто 3") {
		t.Fatalf("summary:\n%s", sum)
	}
}

// On the light route the model never closes a blocking finding; a
// non-blocking one in range it may.
func TestLightRouteNeverResolvesBlocking(t *testing.T) {
	h, routes := routeHarness(t)
	crit := finding.ID("README.md", "Ключ в документации")
	minor := finding.ID("README.md", "Опечатка")
	pos := &core.Position{NewPath: "README.md", OldPath: "README.md", NewLine: 2}
	h.GL.Discussions = []core.Discussion{
		{ID: "d-crit", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(1, finding.NoteBodyOpts(crit, output.Finding{Severity: "critical", Title: "Ключ в документации"}, false, 0, finding.NoteOpts{Blocking: true}), pos)}},
		{ID: "d-minor", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(2, finding.NoteBody(minor, output.Finding{Severity: "minor", Title: "Опечатка"}, false, 0), pos)}},
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, []output.ResolvedFinding{{ID: crit}, {ID: minor}})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(*routes) != 1 || (*routes)[0] != review.RouteLight {
		t.Fatalf("routes %v", *routes)
	}
	if got := strings.Join(h.GL.Resolved, ","); got != "d-minor" {
		t.Fatalf("resolved %s", got)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, summary.GateLine(1, 0)) {
		t.Fatalf("summary:\n%s", sum)
	}
}
