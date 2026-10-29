package review

// Size limit of the review (review.max_mr_lines, review.big_mr): when the
// reviewed range has more changed lines than the limit, the bot runs no
// Claude at all (no review, no verification, no MCP session, no phase-B
// or other model runs) and releases the run booking. It still runs the
// cheap deterministic checks (review.big_mr.deterministic: gitleaks, the todo check and the
// deterministic compat checkers), keeps the trunk-based block, the labels
// and the "could not check" block, and writes a dedicated summary block
// with advice on splitting the MR and how to override the limit.
//
// The reviewed range is what the review would look at: the whole MR on a
// full review, the new commits on an incremental one, so an MR that was
// reviewed once and grows a little keeps getting small incremental
// reviews. The count is the TBD block's (countDiffLines, ignore globs).
//
// Overrides: "@bot review force" (core.JobForceReview, a full review by a
// Developer+, a rerun for the limits) and the label
// review.big_mr.override_label on the MR. With review.big_mr.block the
// bot also opens a merge-blocking thread and resolves it once the MR is
// reviewed (it shrank, or the limit was overridden).

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/labels"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

const (
	// skipTooBig is the OnSkip reason (reviewbot_skipped_total).
	skipTooBig = "too_big"

	overrideForce = "force"
	overrideLabel = "label"

	msgTooBig = "📦 MR слишком большой для ревью: %d %s (порог %d) — Claude не запускался. Советы, как разбить MR, — в сводке. Ревью всё равно: `%s review force`."
)

// diffSize is the size of a `git diff` the way the TBD block counts it:
// added and removed lines, files matching the ignore globs excluded.
func diffSize(diff string, ignore []string) (added, removed, files int) {
	for _, c := range diffcap.Split(diff) {
		if c.Path != "" && diffcap.IgnoredPath(c.Path, ignore) {
			continue
		}
		a, rm := diffcap.CountLines(c.Text)
		added, removed, files = added+a, removed+rm, files+1
	}
	return added, removed, files
}

// sizeGate is the size decision of one review.
type sizeGate struct {
	lines, limit int
	incremental  bool
	// over: the range is bigger than the limit (limit on).
	over bool
	// override: why an over-limit MR is reviewed anyway ("" = it is not).
	override string
	label    string // the override label (for the texts)
}

// skip reports whether Claude must not run.
func (g sizeGate) skip() bool { return g.over && g.override == "" }

// on reports whether the size limit applied to this review.
func (g sizeGate) on() bool { return g.limit > 0 }

// checkMRSize measures the reviewed range (diff is `git diff from..head`
// with the ignore globs applied) against review.max_mr_lines.
func (r *Reviewer) checkMRSize(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, diff string, incremental bool) sizeGate {
	g := sizeGate{limit: eff.BigMR.MaxLines, incremental: incremental, label: eff.BigMR.OverrideLabel}
	if !eff.BigMR.On() {
		return g
	}
	a, rm, _ := diffSize(diff, eff.Ignore)
	g.lines = a + rm
	if g.lines <= g.limit {
		return g
	}
	g.over = true
	log := r.env.Log.With("mr", job.MRKey())
	switch {
	case job.Kind == core.JobForceReview:
		g.override = overrideForce
	case g.label != "" && r.mrHasLabel(ctx, job, g.label):
		g.override = overrideLabel
	}
	if g.override != "" {
		log.Info("review: mr over the size limit, reviewing anyway", "lines", g.lines, "limit", g.limit, "override", g.override)
	}
	return g
}

// mrLabelReader reads the MR's labels (*gitlab.Client).
type mrLabelReader interface {
	MRLabels(ctx context.Context, projectID, mrIID int64) (state string, labels []string, err error)
}

// mrHasLabel reports whether the MR carries label (case-insensitive, like
// GitLab label names). Errors count as "no".
func (r *Reviewer) mrHasLabel(ctx context.Context, job core.Job, label string) bool {
	rd, ok := core.GitLabAs[mrLabelReader](r.env.GitLab)
	if !ok {
		rd = r.labels.Reader() // nil when there is no label client
	}
	if rd == nil {
		return false
	}
	_, mrLabels, err := rd.MRLabels(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		r.env.Log.Warn("review: read mr labels for the size override", "mr", job.MRKey(), "err", err)
		return false
	}
	for _, l := range mrLabels {
		if strings.EqualFold(strings.TrimSpace(l), label) {
			return true
		}
	}
	return false
}

// summaryLine is the summary note of a reviewed over-limit MR ("" when it
// is not over the limit).
func (g sizeGate) summaryLine(mention string) string {
	if !g.over || g.override == "" {
		return ""
	}
	how := fmt.Sprintf("по команде `%s review force`", mention)
	if g.override == overrideLabel {
		how = "по метке " + mdtext.InlineCode(g.label)
	}
	return fmt.Sprintf("📦 MR больше порога ревью (%d %s при пороге %d) — ревью выполнено %s.\n\n",
		g.lines, mdtext.LinesWord(g.lines), g.limit, how)
}

// tooBigRun is what the too-big path needs from the review flow.
type tooBigRun struct {
	mr           *core.MergeRequest
	eff          config.Effective
	co           core.Checkout
	state        *core.MRState
	knownSummary int64
	from, head   string
	incremental  bool
	files        []string
	diff         string
	gate         sizeGate
}

// reviewTooBig finishes a review whose range is over the size limit: no
// Claude run (the booking is released by the caller's defer), the
// deterministic checks, the summary. The reviewed head is NOT advanced: the
// next review looks at the same range again (plus new commits).
func (r *Reviewer) reviewTooBig(ctx context.Context, job core.Job, in tooBigRun) error {
	log := r.env.Log.With("mr", job.MRKey(), "kind", job.Kind)
	log.Info("review: mr too large, claude review skipped", "lines", in.gate.lines, "limit", in.gate.limit,
		"incremental", in.incremental, "from", in.from, "to", in.head)
	if r.onSkip != nil {
		r.onSkip(skipTooBig)
	}
	eff, mr := in.eff, in.mr
	threads, err := r.pub.Scan(ctx, job.ProjectID, job.MRIID, in.knownSummary)
	if err != nil {
		return r.failManual(ctx, job, err)
	}

	// Deterministic checks (review.big_mr.deterministic).
	var cr *compatcheck.Run
	var api *compatcheck.APIRun
	var lr *linters.Run
	if eff.BigMR.Deterministic {
		if eff.Compat.Enabled {
			cr = r.compat.RunChecks(ctx, job, in.co, mr, eff.Compat, eff.Language)
			api = cr.API()
			compatcheck.Candidates(cr, &output.ReviewOutput{}, threads) // deterministic items only: nothing to verify
		}
		lr = r.runSecretsOnly(ctx, job, in.co, mr, eff, in.from, in.diff)
		linters.NoteProblems(ctx, lr)
	}
	idx := r.pub.Index(ctx, job, mr, (cr != nil && len(cr.Items) > 0) || lr.NeedIndex())
	r.compat.Publish(ctx, job, mr, idx, cr, threads)
	r.lint.Publish(ctx, job, mr, idx, lr)
	fixedPending := threads.FixedPendingBlocking
	fixedPending += r.compat.ResolveFixed(ctx, job, cr, &output.ReviewOutput{}, threads)
	fixedPending += r.lint.ResolveFixed(ctx, job, lr)

	// The merge gate: everything still open, plus the size thread, plus
	// what could not be published (fail closed; the reviewed head is never
	// advanced here, so the next review posts it).
	size := r.tooBigThread(ctx, job, eff, in.gate, threads)
	fixedPending += size.pending
	unpublished := size.unpublished + cr.Unpublished() + lr.UnpublishedBlocking()
	blockingOpen := len(threads.OpenCompat) + fixedPending + threads.OpenOtherBlocking + size.open - size.pending + cr.Unpublished() + lr.UnpublishedBlocking()
	if lr != nil && !lr.ScanFailed {
		blockingOpen += lr.BlockingOpen()
	} else {
		blockingOpen += threads.OpenLintBlocking
	}
	if cr != nil {
		blockingOpen += len(cr.Published)
	}
	for _, f := range threads.Open {
		if f.Blocking {
			blockingOpen++
		}
	}

	// Deterministic blocks: trunk-based status, labels (the migration fact
	// is unknown: its analysis feeds the model), what could not be checked.
	ts := r.pipeline.CollectTBD(ctx, job, mr, eff)
	effL := eff
	effL.Migrations.Enabled = false
	labelsBlock := r.labels.Apply(ctx, job, mr, effL, labels.NewPrompt(eff.Labels, in.incremental), labels.Run{Checkout: in.co, Files: in.files,
		Incremental: in.incremental, Compat: cr, API: api, Lint: lr, TBD: ts})
	pr := r.problemsSummary(ctx, eff)

	var b strings.Builder
	b.WriteString(marker.Summary(in.head) + "\n" + summary.Title + "\n\n")
	b.WriteString(summary.GateLine(blockingOpen, fixedPending) + "\n\n")
	if unpublished > 0 {
		b.WriteString(unpublishedLine(unpublished, r.env.Mention()) + "\n\n")
	}
	b.WriteString(renderTooBig(in.gate, eff, r.env.Mention()) + "\n\n")
	if blk := pipeline.RenderTBD(ts, eff.TBD); blk != "" {
		b.WriteString(blk + "\n\n")
	}
	if blk := pipeline.KeptCIBlock(threads.SummaryBody, in.head); blk != "" {
		b.WriteString(blk + "\n\n") // the CI failure block (pipeline/ci.go)
	}
	if pr != nil {
		b.WriteString(pr.block + "\n\n")
	}
	b.WriteString(linters.RenderSecrets(lr) + compatcheck.RenderSection(cr) + compatcheck.RenderAPISummary(api) + linters.RenderSection(lr))
	b.WriteString(labelsBlock)
	b.WriteString(tooBigFooter(in.from, in.head, in.incremental, r.env.Mention()))
	body := mdtext.NeutralizeQuickActions(b.String())
	summaryID, err := r.upsertSummary(ctx, job, threads.SummaryID, r.withPauseLine(ctx, job, body))
	if err != nil {
		log.Error("review: summary note", "err", err)
	} else {
		r.pipeline.WatchPipeline(job, ts, nil, pipeline.CIRunning(eff, mr))
		r.pipeline.WatchCI(job, mr, eff, threads, body)
	}
	r.problemsNotified(ctx, job, pr, err == nil)

	// State: the summary note, the reviewed head unchanged.
	if err == nil && (in.state == nil || in.state.SummaryNoteID != summaryID) {
		s := core.MRState{ProjectID: job.ProjectID, MRIID: job.MRIID, SummaryNoteID: summaryID, UpdatedAt: r.env.Now()}
		if in.state != nil {
			s.ReviewedSHA = in.state.ReviewedSHA
		}
		if err := r.env.Store.SaveMRState(ctx, s); err != nil {
			return fmt.Errorf("save mr state: %w", err)
		}
	}
	if job.Kind.Manual() {
		msg := fmt.Sprintf(msgTooBig, in.gate.lines, mdtext.LinesWord(in.gate.lines), in.gate.limit, r.env.Mention())
		if rerr := r.env.Reply(ctx, job, msg); rerr != nil {
			log.Warn("review: reply too big", "err", rerr)
		}
	}
	return nil
}

// runSecretsOnly runs gitleaks alone plus the in-process todo check (linters.Feature.Run
// adds it); the CI linter reports (lint.ci) are not read for a too-big MR. With
// lint or gitleaks off, only the todo check runs (nil when it is off too).
// Threads of the other tools stay as they are (their tool is "off" for
// this run, see lintToolOn).
func (r *Reviewer) runSecretsOnly(ctx context.Context, job core.Job, co core.Checkout, mr *core.MergeRequest, eff config.Effective, from, diff string) *linters.Run {
	if !eff.Lint.Enabled || !slices.Contains(eff.Lint.Tools, lint.ToolGitleaks) {
		return r.lint.RunTodoOnly(ctx, job, co, mr, eff, from, diff)
	}
	e := eff
	e.Lint.Tools, e.Lint.CI = []string{lint.ToolGitleaks}, nil // CI linter reports are not read either
	return r.lint.Run(ctx, job, co, mr, e, from, diff)
}

// renderTooBig is the summary block of a too-big MR.
func renderTooBig(g sizeGate, eff config.Effective, mention string) string {
	var b strings.Builder
	b.WriteString(marker.TooBigStart + "\n")
	fmt.Fprintf(&b, "### 📦 MR слишком большой для ревью: %d %s (порог %d).\n\n", g.lines, mdtext.LinesWord(g.lines), g.limit)
	if g.incremental {
		b.WriteString("Считались изменения с прошлого ревью (новые коммиты). ")
	}
	b.WriteString("Claude-ревью не проводилось: такой объём нельзя проверить внимательно, а замечания по нему мало кто прочитает. ")
	if eff.BigMR.Deterministic {
		b.WriteString("Выполнены только быстрые проверки без модели: поиск секретов и детерминированные проверки обратной совместимости.\n\n")
	} else {
		b.WriteString("Проверки без модели для больших MR выключены (`review.big_mr.deterministic`).\n\n")
	}
	b.WriteString("Как разбить MR:\n")
	b.WriteString("- **по слоям**: миграции и модели → репозитории/сервисы → API и UI, каждый слой отдельным MR;\n")
	b.WriteString("- **за фича-флагом**: вливайте незаконченную функциональность по частям, выключенной по умолчанию;\n")
	b.WriteString("- **сначала рефакторинг**: переименования, переносы файлов и форматирование — отдельным MR без изменения поведения;\n")
	b.WriteString("- **сгенерированный код отдельно** (или добавьте его в `ignore` в `.reviewbot.yaml` — такие файлы не считаются).\n\n")
	fmt.Fprintf(&b, "Всё равно провести ревью: `%s review force` (Developer+, считается как повторное ревью)", mention)
	if g.label != "" {
		fmt.Fprintf(&b, " или поставьте на MR метку %s и запустите `%s review`", mdtext.InlineCode(g.label), mention)
	}
	b.WriteString(".")
	if eff.BigMR.Block {
		b.WriteString(" Пока MR больше порога, открыт блокирующий тред (`review.big_mr.block`).")
	}
	b.WriteString("\n" + marker.TooBigEnd)
	return b.String()
}

func tooBigFooter(from, to string, incremental bool, mention string) string {
	mode := "полный MR"
	if incremental {
		mode = "новые коммиты"
	}
	return fmt.Sprintf("---\n<sub>Не проверено Claude: `%s..%s` (%s)</sub>\n\n<sub>Команды: %s</sub>\n",
		mdtext.ShortSHA(from), mdtext.ShortSHA(to), mode, summary.CommandsInline(mention))
}
