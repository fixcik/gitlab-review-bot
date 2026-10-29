package gitlab

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestListPipelineJobsDetails(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{{"id": 5, "name": "build", "status": "failed", "web_url": "https://g/j/5",
			"stage": "build", "failure_reason": "script_failure", "allow_failure": true}})
	})
	jobs, err := c.ListPipelineJobs(context.Background(), 42, 9)
	want := core.CIJob{ID: 5, Name: "build", Status: "failed", WebURL: "https://g/j/5", Stage: "build", FailureReason: "script_failure", AllowFailure: true}
	if err != nil || len(jobs) != 1 || jobs[0] != want {
		t.Fatalf("%+v %v", jobs, err)
	}
}

func TestGetJobTraceKeepsTheTail(t *testing.T) {
	log := strings.Repeat("noise line\n", 20000) + "FAIL: TestX\nexit 1\n"
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/42/jobs/5/trace" {
			w.WriteHeader(http.StatusForbidden)
			writeJSON(w, map[string]any{"message": "403 Forbidden"})
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(log))
	})
	b, err := c.GetJobTrace(context.Background(), 42, 5, 100)
	if err != nil || len(b) != 100 || !strings.HasSuffix(string(b), "FAIL: TestX\nexit 1\n") {
		t.Fatalf("%q %v", b, err)
	}
	if f.requests()[0].Method != http.MethodGet {
		t.Error("trace must be a GET")
	}
	if _, err := c.GetJobTrace(context.Background(), 42, 6, 100); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("want ErrForbidden, got %v", err)
	}
	if _, err := c.GetJobTrace(context.Background(), 42, 5, 0); err == nil {
		t.Error("tail 0 must be refused")
	}
	// Short logs come back whole.
	b, err = c.GetJobTrace(context.Background(), 42, 5, int64(len(log)+10))
	if err != nil || string(b) != log {
		t.Fatalf("whole log: %d %v", len(b), err)
	}
}

func TestReadTail(t *testing.T) {
	for _, n := range []int64{1, 7, 100, 1 << 16} {
		in := strings.Repeat("abcdefghij", 10000)
		b, err := readTail(strings.NewReader(in), n)
		if err != nil {
			t.Fatal(err)
		}
		want := in
		if int64(len(in)) > n {
			want = in[int64(len(in))-n:]
		}
		if string(b) != want {
			t.Fatalf("n=%d: got %d bytes", n, len(b))
		}
	}
}

func TestTestReports(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/42/pipelines/9/test_report_summary":
			writeJSON(w, map[string]any{
				"total": map[string]any{"count": 5, "success": 3, "failed": 1, "error": 1, "skipped": 0},
				"test_suites": []map[string]any{{"name": "pytest", "total_count": 5, "failed_count": 1, "error_count": 1,
					"build_ids": []int64{77}, "suite_error": nil}},
			})
		case "/api/v4/projects/42/pipelines/9/test_report":
			writeJSON(w, map[string]any{
				"total_count": 5, "failed_count": 1, "error_count": 1,
				"test_suites": []map[string]any{{"name": "pytest", "total_count": 5, "failed_count": 1, "error_count": 1,
					"build_ids": []int64{77},
					"test_cases": []map[string]any{
						{"status": "success", "name": "ok"},
						{"status": "failed", "name": "test_total", "classname": "tests.test_pricing", "file": "tests/test_pricing.py",
							"system_output": "AssertionError: 114 != 113", "stack_trace": nil},
						{"status": "error", "name": "test_boot", "classname": "tests.test_app", "system_output": []string{"a", "b"}},
					}}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	s, err := c.GetTestReportSummary(context.Background(), 42, 9)
	if err != nil || s.Failed != 1 || s.Errors != 1 || len(s.Suites) != 1 || s.Suites[0].BuildIDs[0] != 77 {
		t.Fatalf("%+v %v", s, err)
	}
	rep, err := c.GetTestReport(context.Background(), 42, 9)
	if err != nil || len(rep.Suites) != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	cs := rep.Suites[0].Cases
	if len(cs) != 2 || cs[0].Name != "test_total" || cs[0].File != "tests/test_pricing.py" ||
		cs[0].SystemOutput != "AssertionError: 114 != 113" || cs[1].SystemOutput != "a\nb" {
		t.Fatalf("cases %+v", cs)
	}
}

func TestListPipelinesAndCompare(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/42/pipelines":
			writeJSON(w, []map[string]any{{"id": 12, "sha": "abc1234", "ref": "main", "status": "success", "source": "push",
				"updated_at": "2026-10-24T10:00:00Z"}})
		case "/api/v4/projects/42/pipelines/12":
			writeJSON(w, map[string]any{"id": 12, "sha": "abc1234", "ref": "main", "status": "failed"})
		case "/api/v4/projects/42/repository/compare":
			writeJSON(w, map[string]any{"commits": []map[string]any{{"id": "aaa1111", "title": "one"}, {"id": "bbb2222", "title": "two"}}})
		case "/api/v4/projects/42/repository/commits/bbb2222/merge_requests":
			writeJSON(w, []map[string]any{{"iid": 7, "state": "merged", "title": "feat: x", "target_branch": "main",
				"author": map[string]any{"username": "alice"}, "merged_by": map[string]any{"username": "old"},
				"merge_user": map[string]any{"username": "bob"}, "merged_at": "2026-10-24T09:00:00Z"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()
	ps, err := c.ListPipelines(ctx, 42, PipelineQuery{Ref: "main", Status: "success", UpdatedAfter: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Limit: 1})
	if err != nil || len(ps) != 1 || ps[0].ID != 12 || ps[0].Source != "push" || ps[0].UpdatedAt.IsZero() {
		t.Fatalf("%+v %v", ps, err)
	}
	q := f.requests()[0].Query
	for _, want := range []string{"ref=main", "status=success", "per_page=1", "updated_after=2026-10-01T00%3A00%3A00Z"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %q", q, want)
		}
	}
	if _, err := c.ListPipelines(ctx, 42, PipelineQuery{Limit: 100, Page: 3}); err != nil {
		t.Fatal(err)
	}
	if q := f.requests()[1].Query; !strings.Contains(q, "page=3") || !strings.Contains(q, "per_page=100") {
		t.Errorf("page query %q", q)
	}
	p, err := c.GetPipeline(ctx, 42, 12)
	if err != nil || p.Status != "failed" {
		t.Fatalf("%+v %v", p, err)
	}
	cs, err := c.CompareCommits(ctx, 42, "aaa0000", "bbb2222")
	if err != nil || len(cs) != 2 || cs[1].SHA != "bbb2222" {
		t.Fatalf("%+v %v", cs, err)
	}
	if _, err := c.CompareCommits(ctx, 42, "main", "bbb2222"); err == nil {
		t.Error("refs must be refused")
	}
	mrs, err := c.ListCommitMRs(ctx, 42, "bbb2222")
	if err != nil || len(mrs) != 1 || mrs[0].MergedBy != "bob" || mrs[0].Author != "alice" || mrs[0].MergedAt.IsZero() {
		t.Fatalf("%+v %v", mrs, err)
	}
	if _, err := c.ListCommitMRs(ctx, 42, "../x"); err == nil {
		t.Error("bad sha must be refused")
	}
}

func TestCreateCommitCommentPolicy(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"note": "x"})
	})
	ctx := context.Background()
	// Default policy (MR comments only): refused before sending.
	if err := c.CreateCommitComment(ctx, 42, "abc1234", "hi"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("default policy: %v", err)
	}
	if len(f.requests()) != 0 {
		t.Fatal("refused write was sent")
	}
	c.writes = BotWrites
	if err := c.CreateCommitComment(ctx, 42, "abc1234", "hi @bob"); err != nil {
		t.Fatal(err)
	}
	r := f.requests()[0]
	if r.Method != http.MethodPost || r.RawPath != "/api/v4/projects/42/repository/commits/abc1234/comments" || !strings.Contains(r.Body, `"note":"hi @bob"`) {
		t.Fatalf("%+v", r)
	}
	c.writes = ServiceWrites
	if err := c.CreateCommitComment(ctx, 42, "abc1234", "hi"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("service client must not comment: %v", err)
	}
	for _, tc := range []struct {
		method, path string
		ok           bool
	}{
		{"POST", "/projects/1/repository/commits/abc1234/comments", true},
		{"PUT", "/projects/1/repository/commits/abc1234/comments", false},
		{"POST", "/projects/1/repository/commits/main/comments", false},
		{"POST", "/projects/1/repository/commits/abc1234/statuses", false},
		{"POST", "/projects/1/repository/commits", false},
	} {
		if got := writeAllowed(BotWrites, tc.method, tc.path); got != tc.ok {
			t.Errorf("%s %s: %v", tc.method, tc.path, got)
		}
	}
}

func TestMRIIDFromRef(t *testing.T) {
	for ref, want := range map[string]int64{"refs/merge-requests/7/head": 7, "refs/merge-requests/12/merge": 12, "main": 0, "refs/heads/x": 0} {
		if got := MRIIDFromRef(ref); got != want {
			t.Errorf("%s: %d", ref, got)
		}
	}
}
