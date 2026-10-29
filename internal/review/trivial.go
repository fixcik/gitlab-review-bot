package review

// Trivial incremental pushes (review.skip_trivial_increments): an
// AUTOMATIC incremental review whose new commits (ReviewedSHA..head, ignore
// globs applied) change only whitespace, comments or documentation files
// runs no Claude at all. The bot advances the reviewed head, keeps every
// thread, puts "ℹ️ Инкремент … без значимых изменений …" into the summary's
// status line and still runs the deterministic checks that matter for new
// commits: gitleaks (secrets in the new commits), the todo check (a new
// comment may be a TODO without a task) and the CI linter reports of the new
// head (lint.ci), plus the trunk-based block. Nothing is booked against the
// limits (the decision comes before Limiter.Reserve).
//
// Never skipped: full reviews (first review, "@bot full review", force
// push, draft -> ready) and every manual "@bot review" (the author can
// always force a review of such a push).
//
// A file counts as trivial when it is a documentation file
// (trivialDocGlobs) or lint.CompareCode finds the same code at both ends.
// Added, deleted, renamed, copied, binary files and mode changes are never
// trivial; neither is a file type the comment lexer does not know.
// CompareCode is fail-closed: directive and annotation comments, Python
// strings that are not docstrings and anything the lexer is not sure about
// count as code (see the header of internal/lint/trivial.go).

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

const (
	// skipTrivial is the OnSkip reason (reviewbot_skipped_total).
	skipTrivial = "trivial_increment"

	// trivialMaxFiles / trivialMaxFileBytes bound the comparison: a bigger
	// increment is reviewed as usual.
	trivialMaxFiles     = 50
	trivialMaxFileBytes = 2 << 20
)

// Kinds of a trivial change (summary wording, in this order).
const (
	trivialFormatting = "форматирование"
	trivialComments   = "комментарии"
	trivialDocs       = "документация"
)

// trivialDocGlobs are documentation files: any change to them is trivial
// for the incremental skip. Narrower than review.routing.low_risk_paths
// on purpose: no .txt (requirements.txt), no docs/** (API specs live there).
var trivialDocGlobs = []string{"**/*.md", "**/*.markdown", "**/*.rst", "**/*.adoc", "**/*.asciidoc"}

// trivialHeaderRe: diff header lines of changes that are never trivial.
var trivialHeaderRe = regexp.MustCompile(`(?m)^(new file mode|deleted file mode|rename from|copy from|old mode|new mode|Binary files|GIT binary patch|similarity index)`)

// trivialInc is the classification of an increment.
type trivialInc struct {
	trivial bool
	kinds   map[string]bool
	files   int
	why     string // why it is not trivial (log)
}

// only renders the kinds: "форматирование/комментарии". A trivial
// increment has at least one file and every file adds its kind.
func (t trivialInc) only() string {
	var ks []string
	for _, k := range []string{trivialFormatting, trivialComments, trivialDocs} {
		if t.kinds[k] {
			ks = append(ks, k)
		}
	}
	return strings.Join(ks, "/")
}

// classifyIncrement decides whether the increment from..head (diff is its
// `git diff`, ignore globs applied) is trivial.
func (r *Reviewer) classifyIncrement(ctx context.Context, co core.Checkout, from, head, diff string) trivialInc {
	t := trivialInc{kinds: map[string]bool{}}
	chunks := diffcap.Split(diff)
	if len(chunks) == 0 {
		t.why = "no diff"
		return t
	}
	if len(chunks) > trivialMaxFiles {
		t.why = fmt.Sprintf("%d files", len(chunks))
		return t
	}
	for _, c := range chunks {
		t.files++
		if c.Path == "" {
			t.why = "unknown path"
			return t
		}
		header := c.Text
		if i := strings.Index(header, "\n@@"); i >= 0 {
			header = header[:i]
		}
		if m := trivialHeaderRe.FindString(header); m != "" {
			t.why = c.Path + ": " + m
			return t
		}
		if config.MatchAnyPath(trivialDocGlobs, c.Path) {
			t.kinds[trivialDocs] = true
			continue
		}
		before, err := co.ShowFile(ctx, from, c.Path)
		if err != nil || len(before) > trivialMaxFileBytes {
			t.why = c.Path + ": cannot read the previous version"
			return t
		}
		after, err := co.ShowFile(ctx, head, c.Path)
		if err != nil || len(after) > trivialMaxFileBytes {
			t.why = c.Path + ": cannot read the new version"
			return t
		}
		switch lint.CompareCode(c.Path, before, after) {
		case lint.CodeWhitespace:
			t.kinds[trivialFormatting] = true
		case lint.CodeComments:
			t.kinds[trivialComments] = true
		default: // changed, or identical bytes although the diff shows the file: unexpected
			t.why = c.Path + ": code changed"
			return t
		}
	}
	t.trivial = true
	return t
}

// skipTrivialIncrement reports whether this review may be skipped as a
// trivial increment (automatic, incremental, the switch on).
func (r *Reviewer) skipTrivialIncrement(job core.Job, incremental bool) bool {
	return incremental && job.Kind == core.JobAutoReview && r.env.Cfg.Review.SkipTrivialIncrements
}

// trivialIncrement handles a trivial increment (done = true: the review is
// over, err is its result); otherwise the review goes on.
func (r *Reviewer) trivialIncrement(ctx context.Context, job core.Job, incremental bool, in trivialRun) (done bool, err error) {
	if !r.skipTrivialIncrement(job, incremental) {
		return false, nil
	}
	in.inc = r.classifyIncrement(ctx, in.co, in.from, in.head, in.diff)
	if !in.inc.trivial {
		r.env.Log.Debug("review: increment is not trivial", "mr", job.MRKey(), "why", in.inc.why)
		return false, nil
	}
	return true, r.reviewTrivialIncrement(ctx, job, in)
}

// trivialRun is what the trivial path needs from the review flow.
type trivialRun struct {
	mr           *core.MergeRequest
	eff          config.Effective
	co           core.Checkout
	knownSummary int64
	from, head   string
	diff         string
	inc          trivialInc
}

// reviewTrivialIncrement finishes an automatic incremental review of a
// trivial increment: no Claude run, no booking; gitleaks, the todo check
// and the CI linter reports; the summary's status, gate, TBD and CI lint
// blocks; the reviewed head advances.
func (r *Reviewer) reviewTrivialIncrement(ctx context.Context, job core.Job, in trivialRun) error {
	log := r.env.Log.With("mr", job.MRKey(), "kind", job.Kind)
	log.Info("review: trivial increment, claude review skipped", "from", in.from, "to", in.head,
		"files", in.inc.files, "only", in.inc.only())
	if r.onSkip != nil {
		r.onSkip(skipTrivial)
	}
	eff, mr := in.eff, in.mr
	threads, err := r.pub.Scan(ctx, job.ProjectID, job.MRIID, in.knownSummary)
	if err != nil {
		return err
	}

	// Deterministic checks of the new commits.
	lr := r.runTrivialLint(ctx, job, in.co, mr, eff, in.from, in.diff)
	r.lint.AddCI(ctx, job, mr, eff, lr, threads) // reports of the repository's CI linters (linters/cilint.go)
	linters.NoteProblems(ctx, lr)
	idx := r.pub.Index(ctx, job, mr, lr.NeedIndex())
	r.lint.Publish(ctx, job, mr, idx, lr)
	r.lint.ResolveFixed(ctx, job, lr)

	// The summary: status line, reviewed head, gate line, TBD, CI linters,
	// problems. Everything else (findings, walkthrough, diagram, …) stays.
	status := fmt.Sprintf("%s `%s..%s` без значимых изменений (только %s) — ревью не требуется.",
		summary.TrivialStatusPrefix, mdtext.ShortSHA(in.from), mdtext.ShortSHA(in.head), in.inc.only())
	pr := r.problemsSummary(ctx, eff)
	if pr != nil && !strings.Contains(threads.SummaryBody, marker.ProblemsStart) {
		status += "\n\n" + pr.block
	}
	body := summary.SetStatus(threads.SummaryBody, status, in.head, r.env.Mention())
	body = marker.SetSummarySHA(body, in.head)
	if pr != nil {
		body = marker.ReplaceBlock(body, marker.ProblemsStart, marker.ProblemsEnd, pr.block)
	}
	body, err = r.lint.RecountGate(ctx, job, threads.SummaryID, body)
	if err != nil {
		log.Warn("review: trivial increment: rescan discussions for the gate line", "err", err)
	}
	ts := r.pipeline.CollectTBD(ctx, job, mr, eff)
	if blk := pipeline.RenderTBD(ts, eff.TBD); blk != "" && marker.TBDRe.MatchString(body) {
		body = pipeline.ReplaceTBD(body, blk) // the pipeline line of the new head
	}
	body = mdtext.NeutralizeQuickActions(linters.ReplaceCIBlock(body, linters.RenderCIBlock(lr)))
	summaryID, err := r.upsertSummary(ctx, job, threads.SummaryID, body)
	if err != nil {
		log.Error("review: summary note", "err", err)
		summaryID = threads.SummaryID
		if summaryID == 0 {
			summaryID = in.knownSummary
		}
	} else {
		r.pipeline.WatchPipeline(job, ts, nil, lr.WaitsForCI() || pipeline.CIRunning(eff, mr))
		r.pipeline.WatchCI(job, mr, eff, threads, body)
	}
	r.problemsNotified(ctx, job, pr, err == nil)
	return r.saveState(ctx, job, in.head, summaryID)
}

// runTrivialLint runs gitleaks alone plus the todo check (linters.Feature.Run adds
// it); the CI linter reports are added by the caller. Lint or gitleaks off:
// the todo check only (nil when it is off too). Threads of the other tools
// stay as they are (their tool is "off" for this run, see lintToolOn).
func (r *Reviewer) runTrivialLint(ctx context.Context, job core.Job, co core.Checkout, mr *core.MergeRequest, eff config.Effective, from, diff string) *linters.Run {
	if !eff.Lint.Enabled {
		return r.lint.RunTodoOnly(ctx, job, co, mr, eff, from, diff)
	}
	e := eff
	e.Lint.Tools = nil
	if slices.Contains(eff.Lint.Tools, lint.ToolGitleaks) {
		e.Lint.Tools = []string{lint.ToolGitleaks}
	}
	return r.lint.Run(ctx, job, co, mr, e, from, diff)
}
