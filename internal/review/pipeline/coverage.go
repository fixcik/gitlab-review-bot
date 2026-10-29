package pipeline

// Diff coverage from CI artifacts (review.coverage + .reviewbot.yaml
// coverage): when the MR's head pipeline succeeded, the coverage report(s)
// of the named jobs are downloaded and the coverage of the changed lines is
// shown in a collapsed summary block (between markers, refreshed by
// JobRefreshStatus once a running pipeline finishes). Claude gets the
// numbers and the uncovered changed lines per file in the system prompt
// (or from the MCP coverage tool). Nothing is run by the bot or by Claude:
// CI already did it, the bot parses the report (internal/diffcov).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diffcov"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

const (
	// covMaxRows / covMaxRanges cap the summary table.
	covMaxRows   = 30
	covMaxRanges = 10
)

// CoverageState is the outcome of a coverage run.
type CoverageState int

const (
	covWaiting     CoverageState = iota // pipeline for the head not finished (or not created yet)
	covPipelineBad                      // pipeline finished without success
	covError                            // job/artifact/report problem
	// CoverageOK: the coverage of the changed lines is known (Res).
	CoverageOK
)

type covReport struct {
	Job  core.CIJob
	Path string
	Rep  *diffcov.Report
}

// CoverageRun is the coverage of one MR head. Res is set when State is
// CoverageOK.
type CoverageRun struct {
	State    CoverageState
	reason   string // covPipelineBad / covError
	warnings []string
	pipeline *core.Pipeline
	reports  []covReport
	changed  diffcov.Changed
	format   diffcov.Format
	Res      diffcov.Result
	min      float64
}

// CollectCoverage returns the coverage of the MR head; nil when coverage is
// not configured for the repo.
func (pf *Feature) CollectCoverage(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective) *CoverageRun {
	ctx, tsp := tracing.Start(ctx, "review.coverage")
	defer tsp.End()
	if !eff.Coverage.Enabled {
		return nil
	}
	ci, ok := core.GitLabAs[cijob.Artifacts](pf.env.GitLab)
	if !ok {
		return nil
	}
	cr := &CoverageRun{pipeline: mr.Pipeline, min: eff.Coverage.Min}
	p := mr.Pipeline
	switch {
	case p == nil:
		cr.State, cr.reason = covError, "у MR нет пайплайна — отчёт о покрытии взять неоткуда"
		return cr
	case p.SHA != "" && mr.DiffRefs.HeadSHA != "" && !strings.EqualFold(p.SHA, mr.DiffRefs.HeadSHA), cijob.Running(p):
		cr.State = covWaiting
		return cr
	case p.Status != "success":
		cr.State, cr.reason = covPipelineBad, "пайплайн "+pipelineStatusRu(p.Status)+" — отчёта о покрытии нет"
		return cr
	}
	log := pf.env.Log.With("mr", job.MRKey())
	jobs, err := ci.ListPipelineJobs(ctx, job.ProjectID, p.ID)
	if err != nil {
		log.Warn("review: coverage: list pipeline jobs", "err", err)
		cr.State, cr.reason = covError, "не удалось получить джобы пайплайна"
		return cr
	}
	max := pf.env.Cfg.Review.Coverage.MaxReportBytes()
	for _, want := range eff.Coverage.Reports {
		matched := matchJobs(jobs, want.Job)
		if len(matched) == 0 {
			cr.warnings = append(cr.warnings, "джоба "+mdtext.InlineCode(want.Job)+" не найдена в пайплайне или завершилась не успешно")
			continue
		}
		for _, j := range matched {
			rep, warning := fetchCoverageReport(ctx, ci, job.ProjectID, j, want.Path, max, log)
			if warning != "" {
				cr.warnings = append(cr.warnings, warning)
				continue
			}
			cr.reports = append(cr.reports, covReport{Job: j, Path: want.Path, Rep: rep})
		}
	}
	if len(cr.reports) == 0 {
		cr.State, cr.reason = covError, "отчёт не получен"
		return cr
	}
	diffs, err := pf.env.GitLab.GetMRDiffs(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: coverage: get mr diffs", "err", err)
		cr.State, cr.reason = covError, "не удалось получить дифф MR"
		return cr
	}
	cr.changed = diffcov.Changed{}
	for _, d := range diffs {
		if d.DeletedFile || diffcap.IgnoredPath(d.NewPath, eff.Ignore) {
			continue
		}
		if lines := diffcov.ChangedFromHunks(d.Diff); len(lines) > 0 {
			cr.changed[d.NewPath] = lines
		}
	}
	reps := make([]*diffcov.Report, 0, len(cr.reports))
	for _, rp := range cr.reports {
		reps = append(reps, rp.Rep)
	}
	merged := diffcov.Merge(reps...)
	cr.format = merged.Format
	cr.Res = diffcov.Compute(merged, cr.changed, nil)
	cr.State = CoverageOK
	log.Info("review: coverage", "percent", fmt.Sprintf("%.1f", cr.Res.Percent()), "covered", cr.Res.Covered, "total", cr.Res.Total, "reports", len(cr.reports))
	return cr
}

// fetchCoverageReport downloads and parses the report at path from job j;
// on failure it returns the summary warning instead.
func fetchCoverageReport(ctx context.Context, ci cijob.Artifacts, projectID int64, j core.CIJob, path string, max int64, log *slog.Logger) (*diffcov.Report, string) {
	data, err := ci.GetJobArtifact(ctx, projectID, j.ID, path, max)
	if err != nil {
		log.Warn("review: coverage: download artifact", "job", j.Name, "path", path, "err", err)
		switch {
		case errors.Is(err, core.ErrNotFound):
			return nil, "в артефактах джобы " + mdtext.InlineCode(j.Name) + " нет файла " + mdtext.InlineCode(path) + " (или артефакты истекли)"
		case strings.Contains(err.Error(), "too large"):
			return nil, "отчёт " + mdtext.InlineCode(path) + " больше review.coverage.max_report_mb"
		}
		return nil, "не удалось скачать " + mdtext.InlineCode(path) + " из джобы " + mdtext.InlineCode(j.Name)
	}
	rep, err := diffcov.Parse(data)
	if err != nil {
		return nil, "не удалось разобрать " + mdtext.InlineCode(path) + " из джобы " + mdtext.InlineCode(j.Name) + ": формат не распознан"
	}
	return rep, ""
}

// matchJobs finds successful jobs named name, including parallel instances
// ("tests 1/3"). Retried jobs: GitLab lists the latest attempt only.
func matchJobs(jobs []core.CIJob, name string) []core.CIJob {
	var out []core.CIJob
	for _, j := range jobs {
		if j.Status != "success" {
			continue
		}
		if j.Name == name || cijob.IsParallelOf(j.Name, name) {
			out = append(out, j)
		}
	}
	return out
}

func pipelineStatusRu(s string) string {
	switch s {
	case "failed":
		return "упал"
	case "canceled", "canceling":
		return "отменён"
	case "skipped":
		return "пропущен"
	case "manual":
		return "ждёт ручного запуска"
	}
	return mdtext.OneLine(s)
}

func fmtPercent(p float64) string {
	if p == float64(int(p)) {
		return strconv.Itoa(int(p)) + "%"
	}
	return strconv.FormatFloat(p, 'f', 1, 64) + "%"
}

// RenderCoverage renders the block with markers ("" when cr is nil).
func RenderCoverage(cr *CoverageRun) string {
	if cr == nil {
		return ""
	}
	const title = "🧪 Покрытие нового кода"
	var b strings.Builder
	switch cr.State {
	case covWaiting:
		b.WriteString("**" + title + ":** ⏳ жду завершения пайплайна")
	case covPipelineBad, covError:
		b.WriteString("**" + title + ":** ⚠️ " + cr.reason)
		for _, w := range cr.warnings {
			b.WriteString("\n- " + w)
		}
	case CoverageOK:
		res := cr.Res
		head := "**" + title + ":** "
		if res.Total == 0 {
			head += "в изменённых строках нет кода, который учитывает отчёт"
		} else {
			head += fmt.Sprintf("%s (%d из %d %s)", fmtPercent(Round1(res.Percent())), res.Covered, res.Total, mdtext.Plural(res.Total, "строки", "строк", "строк"))
			if cr.min > 0 && res.Percent() < cr.min {
				head += " — ⚠️ ниже " + fmtPercent(cr.min)
			}
		}
		b.WriteString("<details>\n<summary>" + strings.ReplaceAll(head, "**", "") + "</summary>\n\n")
		files := leastCoveredFirst(res.Files)
		if len(files) > 0 {
			b.WriteString("| Файл | Покрыто | Не покрыты строки |\n|---|---|---|\n")
			for i, f := range files {
				if i == covMaxRows {
					fmt.Fprintf(&b, "| и ещё %d %s | | |\n", len(files)-covMaxRows, mdtext.Plural(len(files)-covMaxRows, "файл", "файла", "файлов"))
					break
				}
				unc := "—"
				if len(f.Uncovered) > 0 {
					unc = mdtext.TableCell(diffcov.RangesText(f.Uncovered, covMaxRanges))
				}
				fmt.Fprintf(&b, "| %s | %s (%d/%d) | %s |\n", mdtext.TableCell(mdtext.InlineCode(f.Path)), fmtPercent(Round1(f.Percent())), f.Covered, f.Total, unc)
			}
			b.WriteString("\n")
		}
		if len(res.NotInReport) > 0 {
			b.WriteString("Нет в отчёте: " + mdtext.InlineCodeList(res.NotInReport, covMaxRows) + " — тесты их не выполняли, файлы исключены из покрытия или путь не совпал.\n\n")
		}
		for _, w := range cr.warnings {
			b.WriteString("⚠️ " + w + "\n\n")
		}
		b.WriteString("<sub>" + covSource(cr) + "</sub>\n</details>")
	}
	return summary.Block(marker.CoverageStart+"\n", b.String(), "\n"+marker.CoverageEnd) // paths from the report
}

// leastCoveredFirst returns a copy of files ordered by coverage, lowest
// first (stable).
func leastCoveredFirst(files []diffcov.FileResult) []diffcov.FileResult {
	out := append([]diffcov.FileResult(nil), files...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Percent() < out[j].Percent() })
	return out
}

// Round1 rounds a percentage to one decimal place.
func Round1(p float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(p, 'f', 1, 64), 64)
	return v
}

func covSource(cr *CoverageRun) string {
	var jobs []string
	seen := map[string]bool{}
	for _, r := range cr.reports {
		name := mdtext.InlineCode(r.Job.Name)
		if mdtext.IsHTTPURL(r.Job.WebURL) {
			name = "[" + mdtext.OneLine(r.Job.Name) + "](" + r.Job.WebURL + ")"
		}
		if !seen[name] {
			seen[name] = true
			jobs = append(jobs, name)
		}
	}
	s := "Отчёт: " + strings.Join(jobs, ", ")
	if p := cr.pipeline; p != nil && p.ID > 0 {
		pl := fmt.Sprintf("#%d", p.ID)
		if mdtext.IsHTTPURL(p.WebURL) {
			pl = "[" + pl + "](" + p.WebURL + ")"
		}
		s += " · пайплайн " + pl
	}
	if cr.format != "" {
		s += " · " + string(cr.format)
	}
	return s
}

// replaceCoverage swaps the block in a summary body; without a block it
// goes after the TBD block, else before the compat section, else at the end.
func replaceCoverage(body, block string) string {
	return summary.SwapBlock(body, block, marker.CoverageRe, summary.AfterTBD, summary.BeforeCompat)
}

// covPromptFiles / covPromptRanges cap the uncovered lines in the prompt.
const (
	covPromptFiles  = 20
	covPromptRanges = 10
)

// CoverageForClaude is the system prompt note on the coverage of the
// changed lines ("" unless the coverage is known): the totals and the
// uncovered changed lines per file, least covered first, capped. The bot
// computed them from the CI report (internal/diffcov); Claude runs nothing.
// With the MCP coverage tool the note is replaced by a summary that points
// to the tool (the mcp package's CoverageNote).
func CoverageForClaude(cr *CoverageRun) string {
	if cr == nil || cr.State != CoverageOK {
		return ""
	}
	res := cr.Res
	var b strings.Builder
	b.WriteString(CoveragePromptHeading)
	if res.Total == 0 {
		b.WriteString(CoveragePromptNoLines)
	} else {
		fmt.Fprintf(&b, "CI coverage report: %.1f%% of the changed executable lines are covered (%d of %d).", res.Percent(), res.Covered, res.Total)
		files := leastCoveredFirst(res.Files)
		var lines []string
		more := 0
		for _, f := range files {
			if len(f.Uncovered) == 0 {
				continue
			}
			if len(lines) == covPromptFiles {
				more++
				continue
			}
			lines = append(lines, fmt.Sprintf("- %s: %d of %d covered; uncovered changed lines %s", f.Path, f.Covered, f.Total, diffcov.RangesText(f.Uncovered, covPromptRanges)))
		}
		if len(lines) > 0 {
			b.WriteString(" Uncovered changed lines (new line numbers), least covered first:\n" + strings.Join(lines, "\n") + "\n")
			if more > 0 {
				fmt.Fprintf(&b, "- and %d more file(s) with uncovered lines.\n", more)
			}
		} else {
			b.WriteString("\n")
		}
	}
	b.WriteString(CoverageRule)
	return b.String()
}

// The coverage section of the system prompt (with and without MCP).
const (
	CoveragePromptHeading = "\n\n## Test coverage of the changed lines\n"
	CoveragePromptNoLines = "The CI coverage report has no executable lines among the changed lines.\n"
)

// CoverageRule is how Claude uses the coverage (with and without MCP).
const CoverageRule = "When important new logic (branches, error handling, calculations, state changes) has no tests, report it as a finding on that code (minor; major for critical paths such as money, auth, data loss). Do not report uncovered trivial lines (logging, simple getters, wiring).\n"

// NoteCoverageProblems records the bot problems of a coverage run: no
// report (job / artifact / parse), or a report that has none of the changed
// files. A pipeline that is still running or failed is the MR's state, not
// a problem of the bot.
func NoteCoverageProblems(ctx context.Context, cov *CoverageRun) {
	if cov == nil {
		return
	}
	plain := func(s string) string { return strings.ReplaceAll(s, "`", "") }
	if cov.State == covError {
		jobctx.BotProblem(ctx, problems.CoverageMissing, plain(cov.reason), strings.Join(cov.warnings, "; "))
		return
	}
	for _, w := range cov.warnings {
		jobctx.BotProblem(ctx, problems.CoverageMissing, plain(w), "")
	}
	if cov.State == CoverageOK && cov.Res.Total == 0 && len(cov.Res.Files) == 0 && len(cov.Res.NotInReport) > 0 {
		jobctx.BotProblem(ctx, problems.CoverageMissing, "в отчёте о покрытии нет изменённых файлов",
			"не найдены: "+strings.Join(mdtext.CapList(cov.Res.NotInReport, 10), ", "))
	}
}
