package review_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

func TestDiffSizeMatchesTBDCount(t *testing.T) {
	git := reviewtest.BigGitDiff("a.go", 3) +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1,3 +1,2 @@\n x\n--- removed sql comment\n-y\n" +
		reviewtest.BigGitDiff("go.sum", 50) + reviewtest.BigGitDiff("vendor/x/y.go", 40)
	ignore := []string{"**/go.sum", "vendor/**"}
	a, rm, files := review.DiffSize(git, ignore)
	if a != 3 || rm != 2 || files != 2 {
		t.Fatalf("diffSize = +%d -%d %d files", a, rm, files)
	}
	// The TBD block counts the same file the same way from GitLab hunks.
	ga, grm := diffcap.CountLines(reviewtest.BigMRDiffs("a.go", 3)[0].Diff)
	if ga != 3 || grm != 0 {
		t.Fatalf("countDiffLines(gitlab) = %d %d", ga, grm)
	}
	if ga, grm := diffcap.CountLines(reviewtest.BigGitDiff("a.go", 3)); ga != 3 || grm != 0 {
		t.Fatalf("countDiffLines(git) = %d %d: headers counted", ga, grm)
	}
}

func TestTooBigMRRunsNoClaude(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Compat.Res = &compat.Result{}
	h.Fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.SecretFinding}, Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h.GL.PublishMakesThreads = true
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || len(h.Lim.Records()) != 0 {
		t.Fatalf("claude ran: %d runs, %d records", len(h.Runner.Reqs), len(h.Lim.Records()))
	}
	if h.Bl.Reserved != 0 || h.Bl.Released != 0 || h.Bl.Committed != 0 {
		t.Fatalf("booking (the size gate comes before the limits): reserved %d released %d committed %d", h.Bl.Reserved, h.Bl.Released, h.Bl.Committed)
	}
	if len(h.Skips) != 1 || h.Skips[0] != review.SkipTooBig {
		t.Fatalf("skip metric: %v", h.Skips)
	}
	// Deterministic checks: compat once, lint with gitleaks only.
	if len(h.Compat.Calls) != 1 {
		t.Fatalf("compat calls: %d", len(h.Compat.Calls))
	}
	if len(h.Fl.Calls) != 1 || strings.Join(h.Fl.Calls[0].Config.Tools, ",") != lint.ToolGitleaks {
		t.Fatalf("lint: %+v", h.Fl.Calls)
	}
	if len(reviewtest.LintThreads(h.Harness)) != 1 {
		t.Fatalf("secret thread: %+v", h.GL.Published)
	}
	sum := h.Summary()
	for _, want := range []string{marker.TooBigStart, "MR слишком большой для ревью: 2100 строк (порог 2000).", "по слоям", "за фича-флагом",
		"сначала рефакторинг", "сгенерированный код", "@reviewbot review force", "`big-mr-ok`", marker.TBDStart, linters.SecretSectionTitle,
		summary.GateBlockedPrefix, "Не проверено Claude: `base0000..head1111` (полный MR)"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
	if strings.Contains(sum, "Статистика") || strings.Contains(sum, "Изменения слишком большие") {
		t.Errorf("review parts in a too-big summary:\n%s", sum)
	}
	st, _ := h.St.Get()
	if st.ReviewedSHA != "" || st.SummaryNoteID == 0 {
		t.Fatalf("state: %+v (the head must not count as reviewed)", st)
	}
	if len(h.GL.Replies) != 0 {
		t.Fatalf("auto review replied: %+v", h.GL.Replies)
	}
}

func TestTooBigManualReviewRepliesAndCountsNothing(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2001)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || len(h.Lim.Records()) != 0 || h.Bl.Reserved != 0 {
		t.Fatalf("runs %d records %d reserved %d", len(h.Runner.Reqs), len(h.Lim.Records()), h.Bl.Reserved)
	}
	r := reviewtest.LastReply(t, h.GL)
	if !strings.Contains(r.Body, "2001 строка (порог 2000)") || !strings.Contains(r.Body, "review force") {
		t.Fatalf("reply: %s", r.Body)
	}
}

func TestTooBigDeterministicOff(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Cfg.Review.BigMR.Deterministic = false
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Compat.Calls)+len(h.Fl.Calls)+len(h.Runner.Reqs) != 0 {
		t.Fatalf("checks ran: compat %d lint %d claude %d", len(h.Compat.Calls), len(h.Fl.Calls), len(h.Runner.Reqs))
	}
	if sum := h.Summary(); !strings.Contains(sum, "review.big_mr.deterministic") || !strings.Contains(sum, marker.TBDStart) {
		t.Fatalf("summary:\n%s", sum)
	}
}

func TestSizeLimitOffAndIgnoredFiles(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Cfg.Review.MaxMRLines = 0
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("limit off: %d runs", len(h.Runner.Reqs))
	}

	// Ignored (generated) files do not count.
	h2 := reviewtest.NewBigHarness(t, 2100)
	h2.GL.RepoConfig = "ignore:\n  - \"gen/**\"\n"
	h2.Repos.Diff += reviewtest.BigGitDiff("cache.go", 3)
	h2.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h2.Runner.Reqs) != 1 || len(h2.Skips) != 0 {
		t.Fatalf("ignored files counted: %d runs, skips %v", len(h2.Runner.Reqs), h2.Skips)
	}
}

func TestSizeLimitIncrementalRange(t *testing.T) {
	// A reviewed MR of 2100 lines (TBD) gets a small push: the increment is
	// reviewed.
	h := reviewtest.NewBigHarness(t, 2100)
	h.St.States[core.MRKey(reviewtest.PID, reviewtest.IID)] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old000000000000", SummaryNoteID: 0}
	h.Repos.Ancestors["old000000000000"] = true
	h.Repos.Diff = reviewtest.BigGitDiff("cache.go", 30)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || h.Repos.DiffFrom[0] != "old000000000000" {
		t.Fatalf("small increment not reviewed: runs %d from %v", len(h.Runner.Reqs), h.Repos.DiffFrom)
	}
	if st, _ := h.St.Get(); st.ReviewedSHA != "head111111111111" {
		t.Fatalf("state %+v", st)
	}

	// A big increment is not; the reviewed head stays.
	h2 := reviewtest.NewBigHarness(t, 2500)
	h2.St.States[core.MRKey(reviewtest.PID, reviewtest.IID)] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old000000000000"}
	h2.Repos.Ancestors["old000000000000"] = true
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h2.Runner.Reqs) != 0 {
		t.Fatal("big increment reviewed")
	}
	st, _ := h2.St.Get()
	if st.ReviewedSHA != "old000000000000" {
		t.Fatalf("reviewed head moved: %+v", st)
	}
	if sum := h2.Summary(); !strings.Contains(sum, "Считались изменения с прошлого ревью") || !strings.Contains(sum, "(новые коммиты)") {
		t.Fatalf("summary:\n%s", sum)
	}
}

func TestReviewForceIgnoresLimit(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	// Even a reviewed MR: force is a full review.
	h.St.States[core.MRKey(reviewtest.PID, reviewtest.IID)] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old000000000000"}
	h.Repos.Ancestors["old000000000000"] = true
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobForceReview)); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || h.Repos.DiffFrom[0] != "base000000000000" {
		t.Fatalf("force: runs %d from %v", len(h.Runner.Reqs), h.Repos.DiffFrom)
	}
	recs := h.Lim.Records()
	if len(recs) != 1 || recs[0].Kind != core.UsageRerun || recs[0].UserID != reviewtest.UserID || recs[0].MRKey != "42!7" {
		t.Fatalf("usage: %+v", recs)
	}
	if h.Bl.LastReq.User == nil || len(h.Lim.UserCalls) != 1 || h.Lim.UserCalls[0] != core.UsageRerun {
		t.Fatalf("per-user limits not applied: %+v %v", h.Bl.LastReq, h.Lim.UserCalls)
	}
	if sum := h.Summary(); !strings.Contains(sum, "MR больше порога ревью (2100 строк при пороге 2000) — ревью выполнено по команде `@reviewbot review force`") {
		t.Fatalf("summary:\n%s", sum)
	}

	// Per-user limit reached: refused, no run.
	h2 := reviewtest.NewBigHarness(t, 2100)
	h2.Lim.User = core.Decision{Allowed: false, Reason: "лимит повторных ревью"}
	if err := h2.R().Handle(context.Background(), reviewtest.ManualJob(core.JobForceReview)); err != nil {
		t.Fatal(err)
	}
	if len(h2.Runner.Reqs) != 0 || !strings.Contains(reviewtest.LastReply(t, h2.GL).Body, "лимит повторных ревью") {
		t.Fatalf("limit ignored: %+v", h2.GL.Replies)
	}

	// Below Developer: refused.
	h3 := reviewtest.NewBigHarness(t, 2100)
	h3.GL.Access = core.AccessReporter
	if err := h3.R().Handle(context.Background(), reviewtest.ManualJob(core.JobForceReview)); err != nil {
		t.Fatal(err)
	}
	if len(h3.Runner.Reqs) != 0 || reviewtest.LastReply(t, h3.GL).Body != env.MsgNoAccess {
		t.Fatalf("access: %+v", h3.GL.Replies)
	}
}

func TestOverrideLabel(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	mrl := &reviewtest.FakeMRLabels{State: "opened", Labels: []string{"Big-MR-OK"}}
	h.Deps.Labels = &reviewtest.FakeLabelClient{Mr: mrl}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatal("override label ignored")
	}
	if sum := h.Summary(); !strings.Contains(sum, "ревью выполнено по метке `big-mr-ok`") {
		t.Fatalf("summary:\n%s", sum)
	}

	// Label off in the config: not even read.
	h2 := reviewtest.NewBigHarness(t, 2100)
	h2.Cfg.Review.BigMR.OverrideLabel = ""
	lc := &reviewtest.FakeLabelClient{Mr: &reviewtest.FakeMRLabels{State: "opened", Labels: []string{"big-mr-ok"}}}
	h2.Deps.Labels = lc
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h2.Runner.Reqs) != 0 || lc.Reads != 0 {
		t.Fatalf("label override off: runs %d reads %d", len(h2.Runner.Reqs), lc.Reads)
	}

	// A small MR never reads the labels.
	h3 := reviewtest.New(t)
	lc3 := &reviewtest.FakeLabelClient{Mr: &reviewtest.FakeMRLabels{State: "opened"}}
	h3.Deps.Labels = lc3
	h3.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h3.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if lc3.Reads != 0 {
		t.Fatalf("labels read for a small MR: %d", lc3.Reads)
	}
}

func TestRepoLimitOnlyStricter(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 300)
	h.GL.RepoConfig = "max_mr_lines: 250\n"
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || !strings.Contains(h.Summary(), "300 строк (порог 250)") {
		t.Fatalf("repo limit: runs %d\n%s", len(h.Runner.Reqs), h.Summary())
	}

	h2 := reviewtest.NewBigHarness(t, 2100)
	h2.GL.RepoConfig = "max_mr_lines: 5000\n"
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h2.Runner.Reqs) != 0 {
		t.Fatal("a repository raised the service limit")
	}
}

func TestBlockOptionThreadLifecycle(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Cfg.Review.BigMR.Block = true
	m := reviewtest.WithModerator(h.Harness)
	h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	var gate []reviewtest.DraftNote
	for _, d := range h.GL.Published {
		if strings.Contains(marker.Header(d.Body), marker.TooBigThread) {
			gate = append(gate, d)
		}
	}
	if len(gate) != 1 || !marker.HasBlocking(gate[0].Body) || gate[0].Pos != nil {
		t.Fatalf("size thread: %+v", h.GL.Published)
	}
	if sum := h.Summary(); !strings.Contains(sum, summary.GateBlockedPrefix+" 1 блокирующее замечание") || !strings.Contains(sum, "открыт блокирующий тред") {
		t.Fatalf("summary:\n%s", sum)
	}

	// Still too big: no second thread.
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range h.GL.Published {
		if strings.Contains(marker.Header(d.Body), marker.TooBigThread) {
			n++
		}
	}
	if n != 1 || !strings.Contains(h.Summary(), summary.GateBlockedPrefix+" 1 ") {
		t.Fatalf("threads %d\n%s", n, h.Summary())
	}

	// The MR shrank: reviewed, the thread resolved, the merge not blocked.
	h.Repos.Diff = reviewtest.BigGitDiff("cache.go", 10)
	h.GL.Diffs = reviewtest.BigMRDiffs("cache.go", 10)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatal("small MR not reviewed")
	}
	var threadID string
	for _, d := range h.GL.Discussions {
		if strings.Contains(marker.Header(d.Notes[0].Body), marker.TooBigThread) {
			threadID = d.ID
		}
	}
	if len(m.Resolved) != 1 || m.Resolved[0] != threadID {
		t.Fatalf("resolved %v, thread %s", m.Resolved, threadID)
	}
	if r := reviewtest.LastReply(t, h.GL); r.DiscussionID != threadID || !marker.HasFixed(r.Body) || !strings.Contains(r.Body, "10 строк, порог 2000") {
		t.Fatalf("reply: %+v", r)
	}
	if sum := h.Summary(); !strings.Contains(sum, summary.GateClearLine) {
		t.Fatalf("gate:\n%s", sum)
	}
}

func TestBlockOptionOffNoThread(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.GL.PublishMakesThreads = true
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	for _, d := range h.GL.Published {
		if strings.Contains(d.Body, marker.TooBigThread) {
			t.Fatal("size thread without review.big_mr.block")
		}
	}
	if !strings.Contains(h.Summary(), summary.GateClearLine) {
		t.Fatalf("informational by default:\n%s", h.Summary())
	}
}

func TestBlockOptionPersonResolvedThreadNotReposted(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Cfg.Review.BigMR.Block = true
	h.GL.Discussions = []core.Discussion{{ID: "size", Resolvable: true, Resolved: true,
		Notes: []core.Note{reviewtest.BotNote(500, marker.TooBigThread+"\n"+marker.Blocking+"\n📦 big", nil)}}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	for _, d := range h.GL.Published {
		if strings.Contains(d.Body, marker.TooBigThread) {
			t.Fatal("a thread a person resolved was re-posted")
		}
	}
}

func TestHelpListsReviewForce(t *testing.T) {
	if s := summary.CommandsList("@bot"); !strings.Contains(s, "`@bot review force`") {
		t.Fatalf("help: %s", s)
	}
}

func TestTooBigLabelsKeepDeterministicRules(t *testing.T) {
	lh := reviewtest.NewLabelHarness(t)
	lh.GL.RepoConfig = reviewtest.LabelRepoConfig + "    - name: big\n      when_fact: big_mr\n"
	lh.Mr.Project = append(lh.Mr.Project, "big")
	lh.Mr.Labels = []string{"db-migration", "type::bug"}
	lh.Ls.Added = []string{"db-migration", "type::bug"}
	lh.Repos.Diff = reviewtest.BigGitDiff("gen/schema.go", 2100)
	lh.Repos.Files = []string{"gen/schema.go"}
	lh.GL.Diffs = reviewtest.BigMRDiffs("gen/schema.go", 2100)
	if err := lh.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(lh.Runner.Reqs) != 0 {
		t.Fatal("claude ran")
	}
	// Path and fact rules apply; what needs the model (instruction rules,
	// the migration analysis) is left as it is.
	if got := strings.Join(lh.Labels(), ","); got != "backend,big,db-migration,type::bug" {
		t.Fatalf("labels %s", got)
	}
}

func TestConfigCommandShowsSizeLimit(t *testing.T) {
	h := reviewtest.New(t)
	if err := h.R().Handle(context.Background(), reviewtest.CmdJob(core.JobConfig)); err != nil {
		t.Fatal(err)
	}
	if body := reviewtest.LastReply(t, h.GL).Body; !strings.Contains(body, "| порог размера MR (`max_mr_lines`) | 2000 строк, метка-исключение `big-mr-ok` | по умолчанию |") {
		t.Fatalf("config:\n%s", body)
	}
	h2 := reviewtest.New(t)
	h2.Cfg.Review.BigMR.Block = true
	h2.GL.RepoConfig = "max_mr_lines: 800\n"
	if err := h2.R().Handle(context.Background(), reviewtest.CmdJob(core.JobConfig)); err != nil {
		t.Fatal(err)
	}
	if body := reviewtest.LastReply(t, h2.GL).Body; !strings.Contains(body, "| порог размера MR (`max_mr_lines`) | 800 строк, метка-исключение `big-mr-ok`, блокирует мерж | `.reviewbot.yaml` |") {
		t.Fatalf("config:\n%s", body)
	}
}

// sizeThreadDisc is an open blocking size thread of the bot.
func sizeThreadDisc(id string, noteID int64) core.Discussion {
	return core.Discussion{ID: id, Resolvable: true,
		Notes: []core.Note{reviewtest.BotNote(noteID, marker.TooBigThread+"\n"+marker.Blocking+"\n📦 big", nil)}}
}

// "@bot check" recounts the merge gate: the open size thread still blocks
// the merge, so the gate line must not turn clear.
func TestCheckGateCountsOpenSizeThread(t *testing.T) {
	h := chkHarness(t)
	reviewtest.WithModerator(h)
	h.GL.Discussions = append(h.GL.Discussions[:2], sizeThreadDisc("size", 600)) // summary, f1 (blocking), size thread
	h.Runner.Resps = []reviewtest.RunResp{{Out: checkOut(t, review.CheckVerdict{ID: chkID(chkDataLoss), Status: "fixed", Comment: "a"})}}
	if err := h.R().Handle(context.Background(), checkJob("cmd-disc")); err != nil {
		t.Fatal(err)
	}
	if body := h.GL.Notes[500]; !strings.Contains(body, summary.GateBlockedPrefix+" 1 блокирующее замечание") {
		t.Fatalf("gate must count the open size thread:\n%s", body)
	}
}

// The CI lint refresh recounts the gate the same way.
func TestCILintRefreshGateCountsOpenSizeThread(t *testing.T) {
	h, _, _ := reviewtest.CIHarness(t)
	h.Cfg.Review.BlockMinSeverity = "major"
	h.GL.RepoConfig = "lint:\n  ci:\n    - job: semgrep\n      artifact: gl-sast.sarif\n      blocking: true\n"
	var queued []core.Job
	h.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	h.GL.Jobs[0].Status = "running"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sid := h.GL.CreatedNotes[0]
	h.GL.Mu.Lock()
	h.GL.Jobs[0].Status = "success"
	h.GL.Discussions = append(h.GL.Discussions, core.Discussion{ID: "s", Notes: []core.Note{reviewtest.BotNote(sid, h.GL.Notes[sid], nil)}},
		sizeThreadDisc("size", 9600))
	h.GL.Mu.Unlock()
	if err := h.R().Handle(context.Background(), queued[0]); err != nil {
		t.Fatal(err)
	}
	if sum := h.GL.Notes[sid]; !strings.Contains(sum, summary.GateBlockedPrefix+" 2 блокирующих") {
		t.Errorf("gate after the refresh must count the size thread:\n%s", sum)
	}
}

func TestScanDiscussionsCountsBlockingThreadsByKind(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.Discussions = []core.Discussion{
		sizeThreadDisc("size", 600),
		{ID: "other", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(601, "<!-- reviewbot:future-kind -->\n"+marker.Blocking+"\nx", nil)}},
		{ID: "closed", Resolvable: true, Resolved: true, Notes: []core.Note{reviewtest.BotNote(602, marker.TooBigThread+"\n"+marker.Blocking+"\nx", nil)}},
		{ID: "info", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(603, "<!-- reviewbot:future-kind -->\nnot blocking", nil)}},
		// Untrusted text below the header cannot make a note blocking.
		{ID: "body", Resolvable: true, Notes: []core.Note{reviewtest.BotNote(604, "hello\n"+marker.Blocking, nil)}},
	}
	th, err := h.Internals().Pub.Scan(context.Background(), reviewtest.PID, reviewtest.IID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := linters.GateCount(th); b != 2 {
		t.Fatalf("GateCount = %d", b)
	}
	if th.OpenSizeBlocking != 1 || th.OpenOtherBlocking != 1 {
		t.Fatalf("size %d other %d", th.OpenSizeBlocking, th.OpenOtherBlocking)
	}
}

// review.big_mr.block turned off while the size thread is open: the next
// review (small MR) resolves it instead of leaving it to hold the merge.
func TestBlockOffResolvesOpenSizeThread(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 10)
	m := reviewtest.WithModerator(h.Harness)
	h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h.GL.Discussions = []core.Discussion{sizeThreadDisc("size", 600)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.Resolved, ",") != "size" {
		t.Fatalf("resolved %v", m.Resolved)
	}
	if sum := h.Summary(); !strings.Contains(sum, summary.GateClearLine) {
		t.Fatalf("gate:\n%s", sum)
	}

	// Still too big, block off now: resolved too, the gate does not count it.
	h2 := reviewtest.NewBigHarness(t, 2100)
	m2 := reviewtest.WithModerator(h2.Harness)
	h2.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h2.GL.Discussions = []core.Discussion{sizeThreadDisc("size", 600)}
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(m2.Resolved, ",") != "size" {
		t.Fatalf("too big, block off: resolved %v", m2.Resolved)
	}
	if r := reviewtest.LastReply(t, h2.GL); r.DiscussionID != "size" || !strings.Contains(r.Body, "review.big_mr.block") {
		t.Fatalf("reply %+v", r)
	}
	if sum := h2.Summary(); !strings.Contains(sum, summary.GateClearLine) {
		t.Fatalf("gate:\n%s", sum)
	}
}

// A resolve error other than 403 leaves the size thread open: it still
// counts in the gate line.
func TestSizeThreadResolveErrorKeepsCounting(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 10)
	h.Cfg.Review.BigMR.Block = true
	h.GL.FailResolveFor = "size"
	h.GL.Discussions = []core.Discussion{sizeThreadDisc("size", 600)}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if sum := h.Summary(); !strings.Contains(sum, summary.GateBlockedPrefix+" 1 блокирующее") {
		t.Fatalf("gate:\n%s", sum)
	}
}

// The big change was reverted: the range is empty ("nothing to review"),
// the open size thread is closed and the gate line recounted.
func TestNothingToReviewClosesSizeThread(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 10)
	h.Cfg.Review.BigMR.Block = true
	m := reviewtest.WithModerator(h.Harness)
	h.Repos.Diff, h.Repos.Files = "", nil
	h.St.States[core.MRKey(reviewtest.PID, reviewtest.IID)] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "old000000000000", SummaryNoteID: 500}
	h.Repos.Ancestors["old000000000000"] = true
	h.GL.Notes[500] = marker.Summary("old000000000000") + "\n" + summary.Title + "\n\n" + summary.GateBlockedPrefix + " 1 блокирующее замечание. Исправьте и закройте треды.\n\nтекст\n"
	h.GL.Discussions = []core.Discussion{
		{ID: "sum", Notes: []core.Note{reviewtest.BotNote(500, h.GL.Notes[500], nil)}},
		sizeThreadDisc("size", 600),
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || strings.Join(m.Resolved, ",") != "size" {
		t.Fatalf("runs %d resolved %v", len(h.Runner.Reqs), m.Resolved)
	}
	if sum := h.GL.Notes[500]; !strings.Contains(sum, summary.GateClearLine) {
		t.Fatalf("gate:\n%s", sum)
	}
	if st, _ := h.St.Get(); st.ReviewedSHA != "head111111111111" {
		t.Fatalf("state %+v", st)
	}
}

// Every publish path deletes the leftover drafts first: the too-big review
// (size thread, gitleaks thread) ...
func TestTooBigPathNeverPublishesStaleDrafts(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Cfg.Review.BigMR.Block = true
	h.Fl.Res = &lint.Result{Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h.GL.Drafts = []reviewtest.DraftNote{{Body: "stale draft"}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if reviewtest.StalePublished(h.Harness) || len(h.GL.Published) != 1 {
		t.Fatalf("published %+v", h.GL.Published)
	}

	h2 := reviewtest.NewBigHarness(t, 2100)
	h2.Fl.Res = &lint.Result{Findings: []lint.Finding{reviewtest.SecretFinding}, Tools: reviewtest.Ran(lint.ToolGitleaks)}
	h2.GL.Drafts = []reviewtest.DraftNote{{Body: "stale draft"}}
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if reviewtest.StalePublished(h2.Harness) || len(reviewtest.LintThreads(h2.Harness)) != 1 {
		t.Fatalf("published %+v", h2.GL.Published)
	}
}

// ... and the CI lint refresh after the pipeline.
func TestCILintRefreshNeverPublishesStaleDrafts(t *testing.T) {
	h, _, _ := reviewtest.CIHarness(t)
	var queued []core.Job
	h.R().SetEnqueue(func(j core.Job) { queued = append(queued, j) })
	h.GL.Jobs[0].Status = "running"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOutCompat(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	sid := h.GL.CreatedNotes[0]
	h.GL.Mu.Lock()
	h.GL.Jobs[0].Status = "success"
	h.GL.Discussions = append(h.GL.Discussions, core.Discussion{ID: "s", Notes: []core.Note{reviewtest.BotNote(sid, h.GL.Notes[sid], nil)}})
	h.GL.Drafts = []reviewtest.DraftNote{{Body: "stale draft"}}
	n := len(h.GL.Published)
	h.GL.Mu.Unlock()
	if err := h.R().Handle(context.Background(), queued[0]); err != nil {
		t.Fatal(err)
	}
	if reviewtest.StalePublished(h) || len(h.GL.Published) == n {
		t.Fatalf("published %+v", h.GL.Published[n:])
	}
}

// The size gate is decided before the limits: a too-big MR gets its size
// summary / reply even when the global or the per-user limit is exhausted
// (it runs no Claude), instead of being postponed or denied.
func TestTooBigDecidedBeforeLimits(t *testing.T) {
	h := reviewtest.NewBigHarness(t, 2100)
	h.Lim.Global = core.Decision{RetryAt: reviewtest.TestNow.Add(40 * time.Minute), Reason: "Глобальный лимит ревью исчерпан (15 в час)."}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatalf("too-big MR postponed: %v", err)
	}
	if sum := h.Summary(); !strings.Contains(sum, marker.TooBigStart) || strings.Contains(sum, "Ревью отложено") {
		t.Fatalf("summary:\n%s", sum)
	}
	if h.Bl.Reserved != 0 || len(h.Runner.Reqs) != 0 {
		t.Fatalf("reserved %d runs %d", h.Bl.Reserved, len(h.Runner.Reqs))
	}

	h2 := reviewtest.NewBigHarness(t, 2100)
	h2.Lim.User = core.Decision{Allowed: false, Reason: "лимит повторных ревью"}
	if err := h2.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h2.GL); !strings.Contains(r.Body, "MR слишком большой") {
		t.Fatalf("reply %+v", r)
	}

	// A small MR still books before Claude and is postponed.
	h3 := reviewtest.NewBigHarness(t, 10)
	h3.Lim.Global = h.Lim.Global
	err := h3.R().Handle(context.Background(), reviewtest.AutoJob())
	if _, ok := core.AsRetryLater(err); !ok || len(h3.Runner.Reqs) != 0 {
		t.Fatalf("small MR over the limit: %v, runs %d", err, len(h3.Runner.Reqs))
	}
}
