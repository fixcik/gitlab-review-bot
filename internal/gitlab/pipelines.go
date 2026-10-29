package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Pipelines, job logs, test reports and the commits of a branch: what the
// CI failure digest needs (internal/review/ci*.go). Everything here is a
// read (GET) except CreateCommitComment (WriteCommitComments).

// PipelineInfo is one pipeline of a project.
type PipelineInfo struct {
	ID        int64
	SHA       string
	Ref       string
	Status    string
	Source    string // push, merge_request_event, schedule, web, …
	WebURL    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type apiPipeline struct {
	ID        int64     `json:"id"`
	SHA       string    `json:"sha"`
	Ref       string    `json:"ref"`
	Status    string    `json:"status"`
	Source    string    `json:"source"`
	WebURL    string    `json:"web_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (p apiPipeline) info() PipelineInfo {
	return PipelineInfo{ID: p.ID, SHA: p.SHA, Ref: p.Ref, Status: p.Status, Source: p.Source, WebURL: p.WebURL,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

// PipelineQuery filters ListPipelines. Zero fields are not sent.
type PipelineQuery struct {
	Ref          string
	Status       string
	UpdatedAfter time.Time
	// OrderBy: id (default), updated_at, …; Sort: desc (default) or asc.
	OrderBy, Sort string
	// Limit is the page size (1..100, default 20): one page is read.
	Limit int
	// Page is the page to read (1-based; 0 = the first).
	Page int
}

// ListPipelines returns one page of a project's pipelines. Read-only.
func (c *Client) ListPipelines(ctx context.Context, projectID int64, q PipelineQuery) ([]PipelineInfo, error) {
	v := url.Values{}
	if q.Ref != "" {
		v.Set("ref", q.Ref)
	}
	if q.Status != "" {
		v.Set("status", q.Status)
	}
	if !q.UpdatedAfter.IsZero() {
		v.Set("updated_after", q.UpdatedAfter.UTC().Format(time.RFC3339))
	}
	if q.OrderBy != "" {
		v.Set("order_by", q.OrderBy)
	}
	if q.Sort != "" {
		v.Set("sort", q.Sort)
	}
	n := q.Limit
	if n <= 0 || n > perPage {
		n = 20
	}
	v.Set("per_page", strconv.Itoa(n))
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}
	var items []apiPipeline
	if err := c.getJSON(ctx, c.endpoint(v, "projects", id(projectID), "pipelines"), &items); err != nil {
		return nil, err
	}
	out := make([]PipelineInfo, 0, len(items))
	for _, p := range items {
		out = append(out, p.info())
	}
	return out, nil
}

// GetPipeline returns one pipeline. Read-only.
func (c *Client) GetPipeline(ctx context.Context, projectID, pipelineID int64) (*PipelineInfo, error) {
	var p apiPipeline
	if err := c.getJSON(ctx, c.endpoint(nil, "projects", id(projectID), "pipelines", id(pipelineID)), &p); err != nil {
		return nil, err
	}
	info := p.info()
	return &info, nil
}

// GetJobTrace returns the last tail bytes of a job's log (GET
// /projects/:id/jobs/:job_id/trace). GitLab sends the whole log; at most
// 64 MiB of it is read. A Reporter gets 403 on some projects (public
// pipelines off, or protected job logs): core.ErrForbidden.
func (c *Client) GetJobTrace(ctx context.Context, projectID, jobID int64, tail int64) ([]byte, error) {
	if tail <= 0 {
		return nil, errors.New("gitlab: job trace tail must be positive")
	}
	resp, err := c.do(ctx, request{
		method:            http.MethodGet,
		u:                 c.endpoint(nil, "projects", id(projectID), "jobs", id(jobID), "trace"),
		retryServerErrors: true,
		tail:              tail,
	})
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

// TestReport is a pipeline's unit test report (JUnit reports of its jobs).
type TestReport struct {
	Total, Success, Failed, Skipped, Errors int
	Suites                                  []TestSuite
}

// TestSuite is the report of one job (Name is the job name; parallel jobs
// share a suite).
type TestSuite struct {
	Name                                    string
	Total, Success, Failed, Skipped, Errors int
	BuildIDs                                []int64
	// SuiteError: GitLab could not parse the job's report.
	SuiteError string
	// Cases are the test cases; only failed and errored ones are kept by
	// GetTestReport.
	Cases []TestCase
}

// TestCase is one test of a suite.
type TestCase struct {
	Status    string // success, failed, error, skipped
	Name      string
	Classname string
	File      string
	// SystemOutput is the failure message (GitLab's system_output).
	SystemOutput string
	StackTrace   string
}

// flexString decodes a JSON string, null, or an array of strings (joined).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err == nil {
		*f = flexString(strings.Join(list, "\n"))
		return nil
	}
	*f = ""
	return nil
}

type apiTestCase struct {
	Status       string     `json:"status"`
	Name         string     `json:"name"`
	Classname    string     `json:"classname"`
	File         flexString `json:"file"`
	SystemOutput flexString `json:"system_output"`
	StackTrace   flexString `json:"stack_trace"`
}

type apiTestSuite struct {
	Name         string        `json:"name"`
	TotalCount   int           `json:"total_count"`
	SuccessCount int           `json:"success_count"`
	FailedCount  int           `json:"failed_count"`
	SkippedCount int           `json:"skipped_count"`
	ErrorCount   int           `json:"error_count"`
	BuildIDs     []int64       `json:"build_ids"`
	SuiteError   flexString    `json:"suite_error"`
	TestCases    []apiTestCase `json:"test_cases"`
}

type apiTestReport struct {
	TotalCount   int            `json:"total_count"`
	SuccessCount int            `json:"success_count"`
	FailedCount  int            `json:"failed_count"`
	SkippedCount int            `json:"skipped_count"`
	ErrorCount   int            `json:"error_count"`
	TestSuites   []apiTestSuite `json:"test_suites"`
}

type apiTestReportSummary struct {
	Total struct {
		Count   int `json:"count"`
		Success int `json:"success"`
		Failed  int `json:"failed"`
		Skipped int `json:"skipped"`
		Error   int `json:"error"`
	} `json:"total"`
	TestSuites []apiTestSuite `json:"test_suites"`
}

func (s apiTestSuite) suite() TestSuite {
	return TestSuite{Name: s.Name, Total: s.TotalCount, Success: s.SuccessCount, Failed: s.FailedCount,
		Skipped: s.SkippedCount, Errors: s.ErrorCount, BuildIDs: s.BuildIDs, SuiteError: string(s.SuiteError)}
}

// MaxTestReportBytes caps the full test report (all cases of all suites).
const MaxTestReportBytes = 32 << 20

// GetTestReportSummary returns the counts of a pipeline's test report per
// suite, without the cases (GET /pipelines/:id/test_report_summary, cheap).
// A pipeline without JUnit reports has zero counts.
func (c *Client) GetTestReportSummary(ctx context.Context, projectID, pipelineID int64) (*TestReport, error) {
	var a apiTestReportSummary
	if err := c.getJSON(ctx, c.endpoint(nil, "projects", id(projectID), "pipelines", id(pipelineID), "test_report_summary"), &a); err != nil {
		return nil, err
	}
	r := &TestReport{Total: a.Total.Count, Success: a.Total.Success, Failed: a.Total.Failed, Skipped: a.Total.Skipped, Errors: a.Total.Error}
	for _, s := range a.TestSuites {
		r.Suites = append(r.Suites, s.suite())
	}
	return r, nil
}

// GetTestReport returns a pipeline's full test report (GET
// /pipelines/:id/test_report, at most MaxTestReportBytes), keeping only the
// failed and errored cases.
func (c *Client) GetTestReport(ctx context.Context, projectID, pipelineID int64) (*TestReport, error) {
	resp, err := c.do(ctx, request{
		method:            http.MethodGet,
		u:                 c.endpoint(nil, "projects", id(projectID), "pipelines", id(pipelineID), "test_report"),
		retryServerErrors: true,
		maxBytes:          MaxTestReportBytes,
	})
	if err != nil {
		return nil, err
	}
	var a apiTestReport
	if err := decode(resp.body, &a); err != nil {
		return nil, err
	}
	r := &TestReport{Total: a.TotalCount, Success: a.SuccessCount, Failed: a.FailedCount, Skipped: a.SkippedCount, Errors: a.ErrorCount}
	for _, s := range a.TestSuites {
		ts := s.suite()
		for _, tc := range s.TestCases {
			if tc.Status != "failed" && tc.Status != "error" {
				continue
			}
			ts.Cases = append(ts.Cases, TestCase{Status: tc.Status, Name: tc.Name, Classname: tc.Classname, File: string(tc.File),
				SystemOutput: string(tc.SystemOutput), StackTrace: string(tc.StackTrace)})
		}
		r.Suites = append(r.Suites, ts)
	}
	return r, nil
}

// CommitMR is a merge request that contains a commit.
type CommitMR struct {
	IID          int64
	State        string // opened, merged, closed
	Title        string
	SourceBranch string
	TargetBranch string
	Author       string // username
	// MergedBy is who merged it (merge_user, else the deprecated merged_by).
	MergedBy string
	MergedAt time.Time
	WebURL   string
}

type apiUserRef struct {
	Username string `json:"username"`
}

type apiCommitMR struct {
	IID          int64       `json:"iid"`
	State        string      `json:"state"`
	Title        string      `json:"title"`
	SourceBranch string      `json:"source_branch"`
	TargetBranch string      `json:"target_branch"`
	Author       *apiUserRef `json:"author"`
	MergeUser    *apiUserRef `json:"merge_user"`
	MergedBy     *apiUserRef `json:"merged_by"`
	MergedAt     *time.Time  `json:"merged_at"`
	WebURL       string      `json:"web_url"`
}

// ListCommitMRs returns the merge requests that contain a commit (GET
// /projects/:id/repository/commits/:sha/merge_requests). Read-only.
func (c *Client) ListCommitMRs(ctx context.Context, projectID int64, sha string) ([]CommitMR, error) {
	if !shaRe.MatchString(sha) {
		return nil, fmt.Errorf("gitlab: invalid commit sha %q", sha)
	}
	items, err := list[apiCommitMR](ctx, c, nil, "projects", id(projectID), "repository", "commits", sha, "merge_requests")
	if err != nil {
		return nil, err
	}
	out := make([]CommitMR, 0, len(items))
	for _, m := range items {
		mr := CommitMR{IID: m.IID, State: m.State, Title: m.Title, SourceBranch: m.SourceBranch, TargetBranch: m.TargetBranch, WebURL: m.WebURL}
		if m.Author != nil {
			mr.Author = m.Author.Username
		}
		switch {
		case m.MergeUser != nil && m.MergeUser.Username != "":
			mr.MergedBy = m.MergeUser.Username
		case m.MergedBy != nil:
			mr.MergedBy = m.MergedBy.Username
		}
		if m.MergedAt != nil {
			mr.MergedAt = *m.MergedAt
		}
		out = append(out, mr)
	}
	return out, nil
}

// MaxCompareCommits caps the commits CompareCommits returns (newest kept).
const MaxCompareCommits = 200

type apiCompare struct {
	Commits []apiCommit `json:"commits"`
}

// CompareCommits returns the commits reachable from to and not from from
// (GET /repository/compare, straight=false), oldest first, at most
// MaxCompareCommits (the newest ones).
func (c *Client) CompareCommits(ctx context.Context, projectID int64, from, to string) ([]core.Commit, error) {
	if !shaRe.MatchString(from) || !shaRe.MatchString(to) {
		return nil, errors.New("gitlab: compare needs two commit shas")
	}
	q := url.Values{"from": {from}, "to": {to}, "straight": {"false"}}
	var a apiCompare
	if err := c.getJSON(ctx, c.endpoint(q, "projects", id(projectID), "repository", "compare"), &a); err != nil {
		return nil, err
	}
	items := a.Commits
	if len(items) > MaxCompareCommits {
		items = items[len(items)-MaxCompareCommits:]
	}
	out := make([]core.Commit, 0, len(items))
	for _, it := range items {
		out = append(out, core.Commit{SHA: it.ID, Title: it.Title, Message: it.Message, AuthoredAt: it.AuthoredDate})
	}
	return out, nil
}

// CreateCommitComment comments on a commit (POST
// /projects/:id/repository/commits/:sha/comments, WriteCommitComments). Used
// only to tell people that main broke after a direct push (no MR to write
// in). Not retried on 5xx.
func (c *Client) CreateCommitComment(ctx context.Context, projectID int64, sha, note string) error {
	if !shaRe.MatchString(sha) {
		return fmt.Errorf("gitlab: invalid commit sha %q", sha)
	}
	_, err := c.do(ctx, request{
		method: http.MethodPost,
		u:      c.endpoint(nil, "projects", id(projectID), "repository", "commits", sha, "comments"),
		body:   commitCommentReq{Note: note},
	})
	return err
}

type commitCommentReq struct {
	Note string `json:"note"`
}

var (
	shaRe   = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	mrRefRe = regexp.MustCompile(`^refs/merge-requests/(\d+)/(?:head|merge|train)$`)
)

// MRIIDFromRef returns the MR iid of a merge request pipeline ref
// ("refs/merge-requests/7/head"), 0 for a branch.
func MRIIDFromRef(ref string) int64 {
	m := mrRefRe.FindStringSubmatch(ref)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	return n
}
