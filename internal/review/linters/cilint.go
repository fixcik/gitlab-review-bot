package linters

// Linters from CI reports (.reviewbot.yaml lint.ci): the bot does not run
// linters, it reads the reports the repository's own CI jobs produce in the
// MR head pipeline — a file in the job artifacts (artifact:) or the job's
// GitLab Code Quality report (report: codequality) — and publishes the
// results on the changed lines exactly like its own lint results
// (linters/lint.go: threads, blocking, dedupe, closing fixed ones), as tool
// "ci:<tool>".
//
// Timing: a job that has not finished yet does not hold the review back.
// The summary gets a "⏳ ждём job" line (in a block between markers, with
// the waiting tools in a hidden marker), and the JobRefreshStatus chain of
// pipeline/tbd.go (WatchPipeline) re-reads the reports when the jobs finish
// (RefreshCI, linters/cilint_refresh.go) and publishes / closes the lint
// threads and updates the block — without a new Claude run.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// CIReports is implemented by the GitLab client (*gitlab.Client): a job's
// report artifact by type (lint.ci report: codequality). Looked up with
// core.GitLabAs; without it such entries are unavailable.
type CIReports interface {
	GetJobReport(ctx context.Context, projectID, jobID int64, fileType string, maxBytes int64) ([]byte, error)
}

const (
	// CITitle heads the "🔧 Линтеры из CI" summary block.
	CITitle = "🔧 Линтеры из CI"
	// ciMaxOutside caps the list of results that could not be posted.
	ciMaxOutside = 30
)

// ciEntry is one lint.ci entry in one run.
type ciEntry struct {
	cfg  config.CILint
	st   lint.ToolStatus // Tool = "ci:<tool>"
	jobs []core.CIJob    // matched jobs of the head pipeline
}

// ciLintRun is the CI linters of one MR head.
type ciLintRun struct {
	entries  []ciEntry
	pipeline *core.Pipeline
	findings []lint.Finding
}

// statuses returns the tool statuses in configuration order.
func (c *ciLintRun) statuses() []lint.ToolStatus {
	out := make([]lint.ToolStatus, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.st)
	}
	return out
}

// pending returns the tools whose job has not finished.
func (c *ciLintRun) pending() []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, e := range c.entries {
		if e.st.State == lint.StatePending {
			out = append(out, e.st.Tool)
		}
	}
	return out
}

// ciJobWaiting: the job has not finished (it may still produce a report).
func ciJobWaiting(status string) bool {
	switch status {
	case "created", "pending", "running", "scheduled", "waiting_for_resource", "preparing", "waiting_for_callback", "canceling":
		return true
	}
	return false
}

// collectCILint reads the CI linter reports of the MR head pipeline (nil
// when lint.ci is empty). It never fails: problems are entry statuses.
func (lf *Feature) collectCILint(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective) *ciLintRun {
	if !eff.Lint.Enabled || len(eff.Lint.CI) == 0 {
		return nil
	}
	log := lf.env.Log.With("mr", job.MRKey())
	c := &ciLintRun{pipeline: mr.Pipeline}
	for _, cfg := range eff.Lint.CI {
		c.entries = append(c.entries, ciEntry{cfg: cfg, st: lint.ToolStatus{Tool: lint.CIPrefix + cfg.Tool}})
	}
	all := func(state, reason string) *ciLintRun {
		for i := range c.entries {
			c.entries[i].st.State, c.entries[i].st.Reason = state, reason
		}
		return c
	}
	ci, ok := core.GitLabAs[cijob.Artifacts](lf.env.GitLab)
	if !ok {
		return all(lint.StateUnavailable, "GitLab-клиент бота не читает артефакты CI")
	}
	p := mr.Pipeline
	switch {
	case p == nil:
		return all(lint.StateUnavailable, "у MR нет пайплайна — отчёт линтера взять неоткуда")
	case p.SHA != "" && mr.DiffRefs.HeadSHA != "" && !strings.EqualFold(p.SHA, mr.DiffRefs.HeadSHA):
		return all(lint.StatePending, "пайплайн последнего коммита ещё не создан")
	}
	jobs, err := ci.ListPipelineJobs(ctx, job.ProjectID, p.ID)
	if err != nil {
		log.Warn("review: ci lint: list pipeline jobs", "err", err)
		return all(lint.StateFailed, "не удалось получить джобы пайплайна")
	}
	// The MR diff is read once, for the first entry with a report.
	var changed *lint.Changed
	diffFailed := false
	changedLines := func() *lint.Changed {
		if changed != nil || diffFailed {
			return changed
		}
		diffs, err := lf.env.GitLab.GetMRDiffs(ctx, job.ProjectID, job.MRIID)
		if err != nil {
			log.Warn("review: ci lint: get mr diffs", "err", err)
			diffFailed = true
			return nil
		}
		changed = lint.ChangedFromFileDiffs(diffs, func(p string) bool { return diffcap.IgnoredPath(p, eff.Ignore) })
		return changed
	}
	for i := range c.entries {
		e := &c.entries[i]
		e.jobs = cijob.Match(jobs, e.cfg.Job)
		data, problems := lf.readCIEntry(ctx, job, ci, e)
		if len(data) == 0 {
			continue // e.st says why
		}
		lines := changedLines()
		if lines == nil {
			e.st.State, e.st.Reason = lint.StateFailed, "не удалось получить дифф MR"
			continue
		}
		fs, st := lint.CheckReport(lint.CIReport{Tool: e.st.Tool, Format: e.cfg.Format, Severity: core.Severity(e.cfg.Severity),
			Blocking: e.cfg.Blocking, Data: data}, lines, eff.Lint.SeverityOverrides)
		if st.Reason != "" {
			st.Reason = mdtext.Inline(st.Reason, 300) // parser errors may quote the report
		}
		if len(problems) > 0 && st.State == lint.StateRan {
			st.State, st.Reason = lint.StatePartial, strings.Join(mdtext.CapList(problems, 3), "; ")
		}
		e.st = st
		c.findings = append(c.findings, fs...)
	}
	log.Info("review: ci lint", "entries", len(c.entries), "findings", len(c.findings), "pending", len(c.pending()))
	return c
}

// readCIEntry downloads the reports of an entry's jobs; problems are the
// jobs without one. Without any report it sets the entry status.
func (lf *Feature) readCIEntry(ctx context.Context, job core.Job, ci cijob.Artifacts, e *ciEntry) (data [][]byte, problems []string) {
	if len(e.jobs) == 0 {
		e.st.State, e.st.Reason = lint.StateUnavailable, "job "+mdtext.InlineCode(e.cfg.Job)+" не найдена в пайплайне MR"
		return nil, nil
	}
	if waiting := e.waiting(); len(waiting) > 0 {
		e.st.State, e.st.Reason = lint.StatePending, "ждём job "+strings.Join(waiting, ", ")
		return nil, nil
	}
	for _, j := range e.jobs {
		b, why := lf.fetchCIReport(ctx, job, ci, j, e.cfg)
		if why != "" {
			problems = append(problems, why)
			continue
		}
		data = append(data, b)
	}
	if len(data) == 0 {
		e.st.State, e.st.Reason = lint.StateFailed, strings.Join(mdtext.CapList(problems, 3), "; ")
		if allNotRun(e.jobs) {
			e.st.State = lint.StateUnavailable
		}
	}
	return data, problems
}

// ciJobRan: the job finished by running its script (it may have a report).
func ciJobRan(status string) bool { return status == "success" || status == "failed" }

// allNotRun: none of the jobs ran (skipped, manual, canceled).
func allNotRun(jobs []core.CIJob) bool {
	for _, j := range jobs {
		if ciJobRan(j.Status) {
			return false
		}
	}
	return true
}

// fetchCIReport downloads one job's report; why is a summary line (Russian)
// when there is none.
func (lf *Feature) fetchCIReport(ctx context.Context, job core.Job, ci cijob.Artifacts, j core.CIJob, cfg config.CILint) (data []byte, why string) {
	name := mdtext.InlineCode(j.Name)
	if !ciJobRan(j.Status) {
		return nil, fmt.Sprintf("job %s не выполнялась (%s)", name, ciJobStatusRu(j.Status))
	}
	what := mdtext.InlineCode(cfg.Artifact)
	var err error
	if cfg.Report == config.CIReportCodeQuality {
		what = "отчёта Code Quality"
		rep, ok := core.GitLabAs[CIReports](lf.env.GitLab)
		if !ok {
			return nil, "GitLab-клиент бота не скачивает отчёты Code Quality"
		}
		data, err = rep.GetJobReport(ctx, job.ProjectID, j.ID, config.CIReportCodeQuality, lint.MaxReportBytes)
	} else {
		data, err = ci.GetJobArtifact(ctx, job.ProjectID, j.ID, cfg.Artifact, lint.MaxReportBytes)
	}
	if err == nil {
		return data, ""
	}
	lf.env.Log.Warn("review: ci lint: download report", "mr", job.MRKey(), "job", j.Name, "err", err)
	switch {
	case errors.Is(err, core.ErrReportNeedsArchive):
		return nil, "этот GitLab не отдаёт отчёт Code Quality через API (нужен GitLab 19.4+): добавьте файл отчёта в `artifacts:paths` или укажите `artifact:`"
	case errors.Is(err, core.ErrNotFound) && j.Status == "failed":
		return nil, fmt.Sprintf("job %s упала, %s нет", name, ciWhatGen(cfg, what))
	case errors.Is(err, core.ErrNotFound):
		return nil, fmt.Sprintf("в артефактах job %s нет %s (или артефакты истекли)", name, ciWhatGen(cfg, what))
	case errors.Is(err, core.ErrTooLarge):
		return nil, fmt.Sprintf("отчёт job %s больше %d МБ", name, lint.MaxReportBytes>>20)
	}
	return nil, fmt.Sprintf("не удалось скачать отчёт job %s", name)
}

func ciWhatGen(cfg config.CILint, what string) string {
	if cfg.Report != "" {
		return what
	}
	return "файла " + what
}

func ciJobStatusRu(s string) string {
	switch s {
	case "manual":
		return "ждёт ручного запуска"
	case "skipped":
		return "пропущена"
	case "canceled":
		return "отменена"
	}
	return mdtext.OneLine(s)
}

// AddCI reads the CI linter reports and adds them to the lint run (the
// results are published, deduplicated and closed like the bot's own lint
// results). A CI result that repeats an open Claude finding (same file and
// line, names the rule or the tool) is not posted again.
func (lf *Feature) AddCI(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, lr *Run, t *mrthread.Threads) {
	ctx, tsp := tracing.Start(ctx, "review.lint.ci")
	defer tsp.End()
	if lr == nil {
		return
	}
	c := lf.collectCILint(ctx, job, mr, eff)
	if c == nil {
		return
	}
	lr.ci = c
	if lr.Res == nil {
		return // the lint step could not start: nothing is published this time
	}
	lr.Res.Findings = append(lr.Res.Findings, c.findings...)
	lr.Res.Tools = append(lr.Res.Tools, c.statuses()...)
	markClaudeDuplicates(lr, t)
}

// markClaudeDuplicates treats CI results that repeat an open Claude finding
// as already discussed.
func markClaudeDuplicates(lr *Run, t *mrthread.Threads) {
	if t == nil || lr.Res == nil {
		return
	}
	for _, f := range lr.Res.Findings {
		if !lint.IsCI(f.Tool) {
			continue
		}
		id := f.ID()
		if _, ok := lr.Threads[id]; ok {
			continue
		}
		for _, fid := range t.OpenOrder {
			of := t.Open[fid]
			if DuplicateOf(output.Finding{Path: of.Path, Line: of.Line, Title: of.Title, Body: of.Body}, []lint.Finding{f}, nil) != nil {
				// Not in lr.order: never closed as fixed by this run.
				lr.Threads[id] = Thread{DiscussionID: of.DiscussionID, Tool: f.Tool, Path: of.Path, Line: of.Line, Open: true}
				break
			}
		}
	}
}

// WaitsForCI reports whether a CI linter job of this head has not finished
// (the summary is refreshed when it does).
func (lr *Run) WaitsForCI() bool { return lr != nil && len(lr.ci.pending()) > 0 }

// ciToolOn reports whether tool is a CI linter configured for this review.
func ciToolOn(cfg config.Lint, tool string) bool {
	for _, e := range cfg.CI {
		if lint.CIPrefix+e.Tool == tool {
			return true
		}
	}
	return false
}

// waiting returns the unfinished jobs of the entry as inline code.
func (e ciEntry) waiting() []string {
	var names []string
	for _, j := range e.jobs {
		if ciJobWaiting(j.Status) {
			names = append(names, mdtext.InlineCode(j.Name))
		}
	}
	return names
}
