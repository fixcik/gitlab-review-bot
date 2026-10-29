package linters

import (
	"context"
	"slices"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Thread is a bot lint thread found in the MR.
type Thread struct {
	DiscussionID string
	Tool         string
	Path         string
	Line         int
	Title        string
	Blocking     bool
	Open         bool // unresolved and not declared fixed by the bot
}

// Run is the state of the lint step during one review.
type Run struct {
	Cfg config.Lint
	Res *lint.Result // nil when the run could not start
	err error
	// ScanFailed: the MR threads could not be read, so the open lint
	// threads are unknown here (the gate line falls back to the main scan).
	ScanFailed bool

	Threads map[string]Thread // every bot lint thread, by id
	Order   []string          // ids of open threads, in MR order

	Published   []lint.Finding
	newBlocking int
	skipped     int            // already discussed (thread exists)
	dismissed   []lint.Finding // blocking items whose thread a human resolved
	Outside     []lint.Finding // could not be posted as a thread
	Closed      int
	Remaining   []Thread // open threads still unresolved after this run

	// unpublishedBlocking: blocking results in Outside (not anchored is not
	// a reason for a blocking one: it would be a general thread).
	unpublishedBlocking int

	// ci: the CI linters of this head (linters/cilint.go); skippedCI and closedCI
	// are their part of the counts (shown in their own summary block).
	ci                  *ciLintRun
	skippedCI, closedCI int
}

// skip counts a result whose thread already exists.
func (lr *Run) skip(tool string) {
	if lint.IsCI(tool) {
		lr.skippedCI++
	} else {
		lr.skipped++
	}
}

// Run runs the tools against the merge base (never fails the review).
// diff is `git diff from..HEAD`; it is reused when from is the merge base.
func (lf *Feature) Run(ctx context.Context, job core.Job, co core.Checkout, mr *core.MergeRequest, eff config.Effective, from, diff string) *Run {
	ctx, tsp := tracing.Start(ctx, "review.lint")
	defer tsp.End()
	if !eff.Lint.Enabled {
		return lf.RunTodoOnly(ctx, job, co, mr, eff, from, diff) // linters/todo.go
	}
	lr := &Run{Cfg: eff.Lint, Threads: map[string]Thread{}}
	lf.scanLintThreads(ctx, job, lr)
	base := diffcap.FullDiffBase(mr)
	req := lint.Request{
		Checkout: co, Base: base, Head: mr.DiffRefs.HeadSHA, Ignore: eff.Ignore,
		Config: lint.Config{Tools: eff.Lint.Tools, SeverityOverrides: eff.Lint.SeverityOverrides},
	}
	if from == base {
		req.Diff = diff
	}
	res, err := lf.lintChecker(eff.Lint).Check(ctx, req)
	if err != nil {
		lf.env.Log.Warn("review: lint failed", "mr", job.MRKey(), "err", err)
		lr.err = err
		return lr
	}
	lr.Res = res
	lf.runTodo(ctx, job, co, mr, eff, from, diff, lr) // linters/todo.go
	return lr
}

// scanLintThreads collects the bot's lint threads (one extra discussions
// read; errors leave the map empty, so nothing is auto-closed).
func (lf *Feature) scanLintThreads(ctx context.Context, job core.Job, lr *Run) {
	ds, err := lf.env.GitLab.ListDiscussions(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		lf.env.Log.Warn("review: list discussions for lint threads", "mr", job.MRKey(), "err", err)
		lr.err, lr.ScanFailed = err, true
		return
	}
	for _, d := range ds {
		if len(d.Notes) == 0 {
			continue
		}
		first := d.Notes[0]
		if first.System || !lf.env.IsBotNote(first) {
			continue
		}
		id, tool, ok := marker.ParseLint(first.Body)
		if !ok {
			continue
		}
		if _, dup := lr.Threads[id]; dup {
			continue
		}
		th := Thread{DiscussionID: d.ID, Tool: tool, Title: mrthread.ParseCompatNote(first.Body),
			Blocking: marker.HasBlocking(first.Body), Open: !d.Resolved && !mrthread.FixedByBot(d, lf.env.IsBotNote)}
		th.Path, th.Line = mrthread.NotePathLine(first)
		lr.Threads[id] = th
		if th.Open {
			lr.Order = append(lr.Order, id)
		}
	}
}

// NeedIndex reports whether anything may be anchored.
func (lr *Run) NeedIndex() bool { return lr != nil && lr.Res != nil && len(lr.Res.Findings) > 0 }

// Publish posts every new result as a thread: inline on its line, or,
// for blocking results that cannot be anchored, a general thread naming the
// file and line. Non-blocking results that cannot be anchored are listed in
// the summary. Non-blocking threads are resolved right away with the service
// token, like findings.
func (lf *Feature) Publish(ctx context.Context, job core.Job, mr *core.MergeRequest, idx env.DiffIndex, lr *Run) {
	ctx, tsp := tracing.Start(ctx, "review.publish.lint")
	defer tsp.End()
	if lr == nil || lr.Res == nil || len(lr.Res.Findings) == 0 {
		return
	}
	if lr.err != nil {
		// The existing threads are unknown: posting could duplicate them.
		// The blocking results still block (fail closed).
		lr.Outside = append(lr.Outside, lr.Res.Findings...)
		seen := map[string]bool{}
		for _, f := range lr.Res.Findings {
			if id := f.ID(); !seen[id] && lf.ResultBlocking(f) {
				seen[id] = true
				lr.unpublishedBlocking++
			}
		}
		return
	}
	seen := map[string]bool{}
	var batch []publish.Draft
	var batched []lint.Finding
	nonBlocking := map[string]bool{}
	for _, f := range lr.Res.Findings {
		id := f.ID()
		if seen[id] {
			continue
		}
		seen[id] = true
		blocking := lf.ResultBlocking(f)
		if th, ok := lr.Threads[id]; ok {
			if !th.Open && blocking {
				lr.dismissed = append(lr.dismissed, f) // a human resolved it
			} else {
				lr.skip(f.Tool)
			}
			continue
		}
		var pos *core.Position
		if f.InDiff {
			pos, _, _ = lf.pub.Anchor(idx, mr.DiffRefs, output.Finding{Path: f.Path, Line: f.Line})
		}
		if pos == nil && !blocking {
			lr.Outside = append(lr.Outside, f)
			continue
		}
		batch = append(batch, publish.Draft{Body: NoteBody(id, f, blocking, !blocking, pos == nil), Pos: pos, What: "lint " + id})
		batched = append(batched, f)
	}
	created, err := lf.pub.Drafts(ctx, job, batch)
	for i, f := range batched {
		if !created[i] || err != nil {
			lr.Outside = append(lr.Outside, f)
			if lf.ResultBlocking(f) {
				lr.unpublishedBlocking++ // still blocks the merge
			}
			continue
		}
		lr.Published = append(lr.Published, f)
		if lf.ResultBlocking(f) {
			lr.newBlocking++
		} else {
			nonBlocking[f.ID()] = true
		}
	}
	if err == nil {
		lf.ResolveNonBlocking(ctx, job, nonBlocking)
	}
}

// ResolveNonBlocking resolves just-published non-blocking lint threads
// with the service token.
func (lf *Feature) ResolveNonBlocking(ctx context.Context, job core.Job, ids map[string]bool) {
	lf.pub.ResolveNonBlocking(ctx, job, ids, "lint thread", func(body string) (string, bool) {
		if marker.HasBlocking(body) {
			return "", false
		}
		id, _, ok := marker.ParseLint(body)
		return id, ok
	})
}

// ResolveFixed closes open lint threads whose tool ran completely this
// time and no longer reports them. A tool that was unavailable, failed or
// ran on part of the files never closes anything.
func (lf *Feature) ResolveFixed(ctx context.Context, job core.Job, lr *Run) (pending int) {
	if lr == nil {
		return 0
	}
	current := map[string]bool{}
	if lr.Res != nil {
		for _, f := range lr.Res.Findings {
			current[f.ID()] = true
		}
	}
	for _, id := range lr.Order {
		th := lr.Threads[id]
		fixed := lr.err == nil && lr.Res != nil && lr.Res.ToolOK(th.Tool) && !current[id] && lf.lintToolOn(lr, th.Tool)
		if !fixed {
			lr.Remaining = append(lr.Remaining, th)
			continue
		}
		comment := "проверка " + th.Tool + " больше не находит это замечание"
		switch {
		case th.Tool == lint.ToolGitleaks:
			comment = "gitleaks больше не находит этот секрет в коммитах MR. Если секрет был настоящим, его всё равно нужно отозвать"
		case lint.IsCI(th.Tool):
			comment = "в отчёте " + th.Tool + " из CI для последнего коммита этого замечания больше нет"
		}
		done, unresolved := lf.pub.MarkFixed(ctx, job, th.DiscussionID, comment)
		if !done {
			lr.Remaining = append(lr.Remaining, th)
			continue
		}
		if lint.IsCI(th.Tool) {
			lr.closedCI++
		} else {
			lr.Closed++
		}
		if unresolved && th.Blocking {
			pending++ // still unresolved in GitLab: still blocks
		}
	}
	return pending
}

// lintToolOn reports whether tool is configured for this review (a thread of
// a tool the repository disabled stays as it is).
func (lf *Feature) lintToolOn(lr *Run, tool string) bool {
	if lint.IsCI(tool) {
		return ciToolOn(lr.Cfg, tool)
	}
	return slices.Contains(lr.Cfg.Tools, tool)
}

// BlockingOpen is the number of merge-blocking lint threads open after this
// run (new ones plus earlier ones still open).
func (lr *Run) BlockingOpen() int {
	if lr == nil {
		return 0
	}
	n := lr.newBlocking
	for _, th := range lr.Remaining {
		if th.Blocking {
			n++
		}
	}
	return n
}

// UnpublishedBlocking is the number of blocking results whose thread could
// not be published this run (a GitLab error, or the MR threads could not be
// read): they still block the merge (fail closed).
func (lr *Run) UnpublishedBlocking() int {
	if lr == nil {
		return 0
	}
	return lr.unpublishedBlocking
}

// NoteProblems records as bot problems the tools that were unavailable,
// failed or ran partially, or the whole step that could not start.
func NoteProblems(ctx context.Context, lr *Run) {
	if lr == nil {
		return
	}
	if lr.Res == nil {
		if lr.err != nil {
			jobctx.BotProblem(ctx, problems.LintFailed, "линтеры и сканеры: проверка не запустилась", lr.err.Error())
		}
		return
	}
	for _, st := range lr.Res.Tools {
		noteLintStatus(ctx, st)
	}
}

// noteLintStatus records a tool (or CI linter report) that was unavailable,
// failed or ran partially; other states are not problems (a CI job still
// running included).
func noteLintStatus(ctx context.Context, st lint.ToolStatus) {
	var state string
	switch st.State {
	case lint.StateUnavailable:
		state = "недоступен"
	case lint.StateFailed:
		state = "упал"
	case lint.StatePartial:
		state = "отработал частично"
	default:
		return
	}
	jobctx.BotProblem(ctx, problems.LintFailed, st.Tool+": "+state, st.Reason)
}
