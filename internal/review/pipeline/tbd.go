package pipeline

// Trunk-based development status block of the MR summary (review.tbd):
// how big the MR is, how long the branch lives, how far it is behind the
// target branch and what its head pipeline says. Deterministic (GitLab
// data only, no Claude). The block sits between markers so that
// JobRefreshStatus can re-render it while the pipeline is still running.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

const (
	// tbdRefreshEvery: how often a running pipeline is re-checked.
	tbdRefreshEvery = 2 * time.Minute
	// tbdRefreshFor: stop re-checking a pipeline after this long.
	tbdRefreshFor = 3 * time.Hour
)

// TBDStatus is what the trunk-based block shows (the labels feature reads
// the size too).
type TBDStatus struct {
	Added, Removed, Files int
	SizeKnown             bool
	// SizePartial: GitLab collapsed some diffs (too large), so the
	// counts are a lower bound.
	SizePartial bool
	Age         time.Duration
	AgeKnown    bool
	Behind      int // -1 unknown
	Target      string
	Pipeline    *core.Pipeline // nil: none
	HeadSHA     string
	ShowPipe    bool
}

// CollectTBD gathers the block's data; nil when the block is off. Errors
// only make the affected line "unknown".
func (pf *Feature) CollectTBD(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective) *TBDStatus {
	ctx, tsp := tracing.Start(ctx, "review.tbd")
	defer tsp.End()
	if !eff.TBD.Enabled {
		return nil
	}
	log := pf.env.Log.With("mr", job.MRKey())
	s := &TBDStatus{Behind: mr.DivergedCommits, Target: mr.TargetBranch, Pipeline: mr.Pipeline,
		HeadSHA: mr.DiffRefs.HeadSHA, ShowPipe: eff.TBD.Pipeline}
	if diffs, err := pf.env.GitLab.GetMRDiffs(ctx, job.ProjectID, job.MRIID); err != nil {
		log.Warn("review: tbd: get mr diffs", "err", err)
	} else {
		s.SizeKnown = true
		for _, d := range diffs {
			p := d.NewPath
			if p == "" {
				p = d.OldPath
			}
			if diffcap.IgnoredPath(p, eff.Ignore) {
				continue
			}
			s.Files++
			if strings.TrimSpace(d.Diff) == "" && !d.RenamedFile {
				s.SizePartial = true // collapsed / too large / binary
				continue
			}
			a, rm := diffcap.CountLines(d.Diff)
			s.Added += a
			s.Removed += rm
		}
	}
	if commits, err := pf.env.GitLab.ListMRCommits(ctx, job.ProjectID, job.MRIID); err != nil {
		log.Warn("review: tbd: list commits", "err", err)
	} else {
		var oldest time.Time
		for _, c := range commits {
			if !c.AuthoredAt.IsZero() && (oldest.IsZero() || c.AuthoredAt.Before(oldest)) {
				oldest = c.AuthoredAt
			}
		}
		if oldest.IsZero() {
			oldest = mr.CreatedAt
		}
		if !oldest.IsZero() {
			s.Age, s.AgeKnown = pf.env.Now().Sub(oldest), true
			if s.Age < 0 {
				s.Age = 0
			}
		}
	}
	return s
}

// RenderTBD renders the block (with markers); "" when s is nil.
func RenderTBD(s *TBDStatus, t config.TBD) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("### 🌿 Trunk-based\n\n")

	// Size.
	if s.SizeKnown {
		ge := ""
		if s.SizePartial {
			ge = "≥ "
		}
		total := s.Added + s.Removed
		fmt.Fprintf(&b, "- 📏 %s+%d / −%d %s, %d %s", ge, s.Added, s.Removed,
			mdtext.LinesWord(total), s.Files, mdtext.Plural(s.Files, "файл", "файла", "файлов"))
		if t.MaxLines > 0 && total > t.MaxLines {
			fmt.Fprintf(&b, " — ⚠️ больше %d: такой MR трудно ревьюить, лучше разбить на несколько", t.MaxLines)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("- 📏 размер MR узнать не удалось\n")
	}

	// Age.
	if s.AgeKnown {
		fmt.Fprintf(&b, "- 🕒 ветке %s", humanAge(s.Age))
		if max := t.MaxAge.D(); max > 0 && s.Age > max {
			fmt.Fprintf(&b, " — ⚠️ дольше %s: в TBD ветки живут часы, максимум пару дней", humanAge(max))
		}
		b.WriteString("\n")
	}

	// Behind the target branch.
	target := mdtext.InlineCode(cmp.Or(s.Target, "целевой ветки"))
	switch {
	case s.Behind < 0:
	case s.Behind == 0:
		fmt.Fprintf(&b, "- ↩️ ветка актуальна относительно %s\n", target)
	default:
		fmt.Fprintf(&b, "- ↩️ отстаёт от %s на %d %s", target, s.Behind, mdtext.Plural(s.Behind, "коммит", "коммита", "коммитов"))
		if t.MaxBehind > 0 && s.Behind > t.MaxBehind {
			fmt.Fprintf(&b, " — ⚠️ больше %d: обновите ветку (rebase), иначе проверялся не тот код, что попадёт в %s", t.MaxBehind, target)
		}
		b.WriteString("\n")
	}

	// Pipeline.
	if s.ShowPipe {
		b.WriteString("- " + pipelineLine(s.Pipeline, s.HeadSHA) + "\n")
	}
	return summary.Block(marker.TBDStart+"\n", b.String(), marker.TBDEnd) // the target branch name
}

func pipelineLine(p *core.Pipeline, head string) string {
	if p == nil {
		return "⚪ пайплайна нет"
	}
	link := func(text string) string {
		if mdtext.IsHTTPURL(p.WebURL) {
			return "[" + text + "](" + p.WebURL + ")"
		}
		return text
	}
	stale := ""
	if head != "" && p.SHA != "" && !strings.EqualFold(p.SHA, head) {
		stale = " (для предыдущего коммита)"
	}
	switch p.Status {
	case "success":
		return "✅ " + link("пайплайн прошёл") + stale
	case "failed":
		return "❌ " + link("пайплайн упал") + stale + " — main должен оставаться зелёным"
	case "canceled", "canceling":
		return "⚪ " + link("пайплайн отменён") + stale
	case "skipped":
		return "⚪ " + link("пайплайн пропущен") + stale
	case "manual":
		return "⏸ " + link("пайплайн ждёт ручного запуска") + stale
	}
	if cijob.Running(p) {
		return "⏳ " + link("пайплайн выполняется") + stale
	}
	return "⚪ " + link("пайплайн: "+mdtext.OneLine(p.Status)) + stale
}

func humanAge(d time.Duration) string {
	h := int(d.Round(time.Hour) / time.Hour)
	if h < 48 {
		if h < 1 {
			return "меньше часа"
		}
		return fmt.Sprintf("%d %s", h, mdtext.Plural(h, "час", "часа", "часов"))
	}
	days := h / 24
	return fmt.Sprintf("%d %s", days, mdtext.Plural(days, "день", "дня", "дней"))
}

// ReplaceTBD swaps the block in a summary body; without a block it goes
// before the compat section, else at the end.
func ReplaceTBD(body, block string) string {
	return summary.SwapBlock(body, block, marker.TBDRe, summary.BeforeCompat)
}

// refreshWatch remembers MRs with a status refresh chain, so a review does
// not start a second one while the first is still waiting.
type refreshWatch struct {
	mu    sync.Mutex
	since map[string]time.Time
}

// Start records a chain for key started at now; false when one runs
// already.
func (w *refreshWatch) Start(key string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.since == nil {
		w.since = map[string]time.Time{}
	}
	if _, ok := w.since[key]; ok {
		return false
	}
	w.since[key] = now
	return true
}

// StartedAt returns when the chain of key started (false: none runs).
func (w *refreshWatch) StartedAt(key string) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.since[key]
	return t, ok
}

func (w *refreshWatch) stop(key string) {
	w.mu.Lock()
	delete(w.since, key)
	w.mu.Unlock()
}

// WatchPipeline enqueues a JobRefreshStatus when the block shows a running
// pipeline (one chain per MR).
func (pf *Feature) WatchPipeline(job core.Job, s *TBDStatus, cov *CoverageRun, ciWait bool) {
	if !needsRefresh(s, cov) && !ciWait {
		return
	}
	if !pf.Refreshing.Start(job.MRKey(), pf.env.Now()) {
		return
	}
	if !pf.host.Enqueue(core.Job{Kind: core.JobRefreshStatus, ProjectID: job.ProjectID, ProjectPath: job.ProjectPath,
		MRIID: job.MRIID, EnqueuedAt: pf.env.Now()}) {
		pf.Refreshing.stop(job.MRKey())
	}
}

// needsRefresh: the pipeline line shows a running pipeline, or the
// coverage waits for the head pipeline. (CI linters waiting for their jobs
// are checked by the callers: linters/cilint.go, cilint_refresh.go.)
func needsRefresh(s *TBDStatus, cov *CoverageRun) bool {
	return (s != nil && s.ShowPipe && cijob.Running(s.Pipeline)) || (cov != nil && cov.State == covWaiting)
}

// HandleRefreshStatus re-renders the block. While the pipeline is still
// running it asks the queue to run it again in tbdRefreshEvery (for at
// most tbdRefreshFor). Never runs Claude, never touches limits.
func (pf *Feature) HandleRefreshStatus(ctx context.Context, job core.Job) error {
	key := job.MRKey()
	started, ok := pf.Refreshing.StartedAt(key)
	if !ok {
		started = pf.env.Now()
		pf.Refreshing.Start(key, started)
	}
	again, err := pf.refreshStatus(ctx, job)
	if err == nil && again && pf.env.Now().Sub(started) < tbdRefreshFor {
		return &core.RetryLater{At: pf.env.Now().Add(tbdRefreshEvery), Reason: "pipeline still running"}
	}
	pf.Refreshing.stop(key)
	return err
}

// refreshStatus updates the block; again reports a still-running pipeline.
func (pf *Feature) refreshStatus(ctx context.Context, job core.Job) (again bool, err error) {
	log := pf.env.Log.With("mr", job.MRKey(), "kind", job.Kind)
	mr, err := pf.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return false, fmt.Errorf("get mr: %w", err)
	}
	if mr.State != "opened" {
		return false, nil
	}
	eff, ok, err := pf.env.RepoConfig.Auto(ctx, job.ProjectID, mr)
	if !ok {
		return false, err
	}
	if !eff.Enabled || (!eff.TBD.Enabled && !eff.Coverage.Enabled && len(eff.Lint.CI) == 0 && !eff.CI.Enabled) {
		return false, nil
	}
	threads, err := pf.pub.ScanMR(ctx, job)
	if err != nil {
		return false, err
	}
	if threads.SummaryID == 0 {
		return false, nil
	}
	s := pf.CollectTBD(ctx, job, mr, eff)
	cov := pf.CollectCoverage(ctx, job, mr, eff)
	body := ReplaceTBD(threads.SummaryBody, RenderTBD(s, eff.TBD))
	body = replaceCoverage(body, RenderCoverage(cov))
	body, ciWait := pf.lint.RefreshCI(ctx, job, mr, eff, threads, body) // CI linter reports (linters/cilint_refresh.go)
	body = mdtext.NeutralizeQuickActions(body)
	if body != threads.SummaryBody {
		if err := pf.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, threads.SummaryID, body); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return false, nil
			}
			return false, fmt.Errorf("update summary: %w", err)
		}
		log.Info("review: tbd status refreshed", "pipeline", pipelineStatus(s))
	}
	if !CIRunning(eff, mr) {
		pf.WatchCI(job, mr, eff, threads, body) // the head pipeline finished: CI digest (pipeline/ci.go)
	}
	return needsRefresh(s, cov) || ciWait || CIRunning(eff, mr), nil
}

func pipelineStatus(s *TBDStatus) string {
	if s == nil || s.Pipeline == nil {
		return ""
	}
	return s.Pipeline.Status
}

// TBDRulesNote is the system prompt rule about switching off changed
// behavior ("" when off). The severity is fixed at major: visible, and
// blocking only if review.block_min_severity is lowered to major.
func TBDRulesNote(t config.TBD) string {
	if !t.Enabled || !t.FeatureFlags {
		return ""
	}
	how := "The team has no feature-flag system yet: ANY way to switch the new behavior off without a deploy counts (a config key, an environment variable, a flag, a per-request opt-in), as long as the default keeps the old behavior or flipping it is a config change."
	if h := strings.TrimSpace(t.FlagsHint); h != "" {
		how = "How the team switches behavior: " + mdtext.OneLine(h) + ". Other equivalent switches (config key, environment variable) also count."
	}
	return "\n\n## Trunk-based development: switching off changed behavior\n" +
		"The team uses trunk-based development: every merge is deployed. " +
		"When this merge request CHANGES THE BEHAVIOR of functionality that is already in use — the semantics or response of an existing endpoint, how an existing event or message is handled, what an existing job does, what is written to an existing table or cache, a default value of existing configuration — and the new behavior cannot be switched off without a deploy, report ONE finding per such change with severity \"major\", titled like \"Изменение поведения без возможности выключить\", naming the affected existing callers and suggesting how to put it behind a switch. " +
		how + " " +
		"Do NOT ask for a switch for additive changes nothing uses yet: new endpoints, new optional fields, new event types or consumers, new code paths existing callers cannot reach, refactorings without behavior change, tests, docs. For a plain bug fix that restores the intended behavior, ask only if the fix is risky for existing callers.\n"
}
