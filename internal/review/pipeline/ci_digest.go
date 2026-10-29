package pipeline

// The digest of a failed pipeline (review.ci, docs/features/ci.md): which
// jobs failed and why, from GitLab data only (no Claude). Failed tests come
// from the pipeline's JUnit test report, jobs of lint.ci are referenced
// (their findings are lint threads already), every other failed job gets
// the end of its log: ANSI colors, GitLab section markers, timestamps and
// runner boilerplate removed, secrets scrubbed, capped. Job logs and test
// messages are untrusted: in notes they are fenced or escaped, in prompts
// they sit inside <untrusted_…> tags.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

// CIFailures is implemented by the GitLab client (*gitlab.Client): what the
// CI failure digest reads. Looked up with core.GitLabAs; without it the
// feature does nothing.
type CIFailures interface {
	ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]core.CIJob, error)
	GetJobTrace(ctx context.Context, projectID, jobID int64, tail int64) ([]byte, error)
	GetTestReportSummary(ctx context.Context, projectID, pipelineID int64) (*gitlab.TestReport, error)
	GetTestReport(ctx context.Context, projectID, pipelineID int64) (*gitlab.TestReport, error)
}

const (
	// ciTraceTail: how much of the end of a job log is downloaded.
	ciTraceTail = 512 << 10
	// ciMaxLogJobs: failed jobs whose log is shown (the rest are named).
	ciMaxLogJobs = 6
	// ciMaxLineRunes caps one log line.
	ciMaxLineRunes = 300
	// ciMsgLines / ciMsgRunes cap the message of one failed test.
	ciMsgLines = 5
	ciMsgRunes = 200
)

// Kinds of a failed job in the digest.
const (
	ciKindTests = "tests" // failed cases from the JUnit report
	ciKindLint  = "lint"  // a lint.ci job: its findings are lint threads
	ciKindLog   = "log"   // the end of the job log
)

// ciTestCase is one failed test of the report.
type ciTestCase struct {
	Suite, Name, Classname, File, Status string
	// Message: the first lines of the failure message (cleaned, scrubbed).
	Message string
}

// ciJobDigest is one failed job.
type ciJobDigest struct {
	Job  core.CIJob
	Kind string
	// LintTool is "ci:<tool>" for a lint.ci job.
	LintTool string
	// Tests: failed cases of this job (capped per pipeline); TestsFailed is
	// the job's count of failed and errored cases, TestsTotal of all.
	Tests                   []ciTestCase
	TestsFailed, TestsTotal int
	// Log is the cleaned end of the log; LogNote says why there is none.
	Log     []string
	LogNote string
}

// ciDigest is a failed pipeline.
type ciDigest struct {
	Pipeline core.Pipeline
	// Failed are the failures that count (allow_failure false).
	Failed []ciJobDigest
	// Warnings: failed jobs with allow_failure.
	Warnings []core.CIJob
	// Ignored: failed jobs matching review.ci.ignore_jobs / ci.ignore_jobs.
	Ignored []string
	// MoreTests: failed cases over max_tests.
	MoreTests int
	// Fingerprint identifies "the same failure": job names, failed tests
	// and the normalized end of the logs.
	Fingerprint string
}

// ciRead calls f with the bot's client. Only the bot's own token reads CI:
// what is read is published in MR notes and sent to the model, so it must be
// what the bot's identity may see (a more privileged token would show
// everyone who sees the MR the logs they cannot open themselves).
func (pf *Feature) ciRead(ctx context.Context, f func(CIFailures) error) error {
	cf, ok := core.GitLabAs[CIFailures](pf.env.GitLab)
	if !ok {
		return errors.New("the GitLab client cannot read CI jobs")
	}
	return f(cf)
}

// ciForbiddenHint is the problem detail of a 403 on a CI read.
const ciForbiddenHint = "403: учётной записи бота GitLab не отдаёт это (логи job / отчёт тестов). Дайте ей доступ: роль в проекте, при которой видны job и их логи, " +
	"или видимость CI (Settings → CI/CD → General pipelines → «Project-based pipeline visibility» / «Public pipelines»). " +
	"Служебный токен для чтения CI не используется: прочитанное публикуется в MR — "

// ciFetchProblem records what could not be read (problem ci_fetch_failed).
func ciFetchProblem(ctx context.Context, subject string, err error) {
	detail := ""
	if err != nil {
		detail = err.Error()
		if errors.Is(err, core.ErrForbidden) {
			detail = ciForbiddenHint + detail
		}
	}
	jobctx.BotProblem(ctx, problems.CIFetchFailed, subject, detail)
}

// ciForbiddenNote is the digest line of a job whose log the bot may not read.
func ciForbiddenNote(j core.CIJob) string {
	open := "откройте job в GitLab"
	if mdtext.IsHTTPURL(j.WebURL) {
		open = "откройте job: " + j.WebURL
	}
	return "лог job " + mdtext.InlineCode(j.Name) + " боту недоступен (нет прав) — " + open
}

// buildCIDigest reads the failed jobs of pipeline p. An error means the job
// list itself could not be read (nothing to show).
func (pf *Feature) buildCIDigest(ctx context.Context, projectID int64, p core.Pipeline, eff config.Effective) (*ciDigest, error) {
	var jobs []core.CIJob
	if err := pf.ciRead(ctx, func(c CIFailures) (err error) {
		jobs, err = c.ListPipelineJobs(ctx, projectID, p.ID)
		return err
	}); err != nil {
		ciFetchProblem(ctx, fmt.Sprintf("CI: джобы пайплайна #%d", p.ID), err)
		return nil, fmt.Errorf("list pipeline jobs: %w", err)
	}
	d := &ciDigest{Pipeline: p}
	// The API lists the latest attempt of each job (retried attempts only
	// with include_retried), so a retried job counts with its last result.
	for _, j := range jobs {
		if j.Status != "failed" {
			continue // success, manual, skipped, canceled, running, …
		}
		switch {
		case eff.CI.IgnoredJob(j.Name):
			d.Ignored = append(d.Ignored, j.Name)
		case j.AllowFailure:
			d.Warnings = append(d.Warnings, j)
		default:
			d.Failed = append(d.Failed, ciJobDigest{Job: j})
		}
	}
	sort.SliceStable(d.Failed, func(i, k int) bool { return d.Failed[i].Job.ID < d.Failed[k].Job.ID })
	if len(d.Failed) == 0 {
		d.Fingerprint = ciFingerprint(d)
		return d, nil
	}
	pf.addCITests(ctx, projectID, d, eff.CI.MaxTests)
	logs := 0
	for i := range d.Failed {
		jd := &d.Failed[i]
		if tool := ciLintToolOf(eff, jd.Job); tool != "" {
			jd.Kind, jd.LintTool = ciKindLint, tool
			continue
		}
		if jd.Kind == ciKindTests && len(jd.Tests) > 0 {
			continue
		}
		jd.Kind = ciKindLog
		if logs == ciMaxLogJobs {
			jd.LogNote = fmt.Sprintf("лог не показан: не больше %d job в дайджесте", ciMaxLogJobs)
			continue
		}
		logs++
		pf.addCILog(ctx, projectID, jd, eff.CI.MaxLogLines)
	}
	d.Fingerprint = ciFingerprint(d)
	return d, nil
}

// addCILog reads the end of a failed job's log into jd (LogNote says why
// there is none).
func (pf *Feature) addCILog(ctx context.Context, projectID int64, jd *ciJobDigest, maxLines int) {
	var raw []byte
	err := pf.ciRead(ctx, func(c CIFailures) (err error) {
		raw, err = c.GetJobTrace(ctx, projectID, jd.Job.ID, ciTraceTail)
		return err
	})
	if err != nil {
		pf.env.Log.Warn("review: ci: job log", "job", jd.Job.Name, "err", err)
		ciFetchProblem(ctx, "CI: лог job "+jd.Job.Name, err)
		jd.LogNote = "лог прочитать не удалось"
		if errors.Is(err, core.ErrForbidden) {
			jd.LogNote = ciForbiddenNote(jd.Job)
		}
		return
	}
	jd.Log = cleanCILog(string(raw), maxLines, pf.scrub)
	if len(jd.Log) == 0 {
		jd.LogNote = "лог пуст"
	}
}

// ciLintToolOf returns "ci:<tool>" when the job is one of the repository's
// lint.ci jobs (its findings are published as lint threads).
func ciLintToolOf(eff config.Effective, j core.CIJob) string {
	if !eff.Lint.Enabled {
		return ""
	}
	for _, e := range eff.Lint.CI {
		if len(cijob.Match([]core.CIJob{j}, e.Job)) > 0 {
			return lint.CIPrefix + e.Tool
		}
	}
	return ""
}

// addCITests attaches the failed cases of the pipeline's test report to
// their jobs (by build id, else by suite = job name or its parallel base
// name), at most max cases in all.
func (pf *Feature) addCITests(ctx context.Context, projectID int64, d *ciDigest, max int) {
	var sum *gitlab.TestReport
	err := pf.ciRead(ctx, func(c CIFailures) (err error) {
		sum, err = c.GetTestReportSummary(ctx, projectID, d.Pipeline.ID)
		return err
	})
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			pf.env.Log.Warn("review: ci: test report summary", "err", err)
			ciFetchProblem(ctx, fmt.Sprintf("CI: сводка тестов пайплайна #%d", d.Pipeline.ID), err)
		}
		return
	}
	if sum == nil || sum.Failed+sum.Errors == 0 {
		return
	}
	var rep *gitlab.TestReport
	if err := pf.ciRead(ctx, func(c CIFailures) (err error) {
		rep, err = c.GetTestReport(ctx, projectID, d.Pipeline.ID)
		return err
	}); err != nil {
		pf.env.Log.Warn("review: ci: test report", "err", err)
		ciFetchProblem(ctx, fmt.Sprintf("CI: отчёт тестов пайплайна #%d", d.Pipeline.ID), err)
		return
	}
	shown := 0
	for _, s := range rep.Suites {
		if s.Failed+s.Errors == 0 {
			continue
		}
		i := suiteJob(d.Failed, s)
		if i < 0 {
			continue // a suite of an ignored or allowed-to-fail job
		}
		jd := &d.Failed[i]
		jd.Kind = ciKindTests
		jd.TestsFailed += s.Failed + s.Errors
		jd.TestsTotal += s.Total
		for _, c := range s.Cases {
			if shown >= max {
				d.MoreTests++
				continue
			}
			shown++
			msg := c.SystemOutput
			if strings.TrimSpace(msg) == "" {
				msg = c.StackTrace
			}
			jd.Tests = append(jd.Tests, ciTestCase{Suite: s.Name, Name: mdtext.OneLine(pf.scrub(c.Name)), Classname: mdtext.OneLine(pf.scrub(c.Classname)),
				File: mdtext.OneLine(c.File), Status: c.Status, Message: ciMessage(msg, pf.scrub)})
		}
	}
}

// suiteJob finds the failed job a test suite belongs to (-1: none).
func suiteJob(jobs []ciJobDigest, s gitlab.TestSuite) int {
	for i, jd := range jobs {
		for _, id := range s.BuildIDs {
			if id == jd.Job.ID {
				return i
			}
		}
	}
	for i, jd := range jobs {
		if len(cijob.Match([]core.CIJob{jd.Job}, s.Name)) > 0 {
			return i
		}
	}
	return -1
}

// ciMessage keeps the first lines of a failure message.
func ciMessage(s string, scrub func(string) string) string {
	lines := cleanCILog(s, 0, scrub)
	if len(lines) > ciMsgLines {
		lines = lines[:ciMsgLines]
	}
	for i, l := range lines {
		lines[i] = mdtext.Shorten(l, ciMsgRunes)
	}
	return strings.Join(lines, "\n")
}
