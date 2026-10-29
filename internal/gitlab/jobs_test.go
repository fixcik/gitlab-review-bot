package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestListPipelineJobs(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{{"id": 5, "name": "tests 1/2", "status": "success", "web_url": "https://g/j/5"}})
	})
	jobs, err := c.ListPipelineJobs(context.Background(), 42, 9)
	if err != nil || len(jobs) != 1 || jobs[0] != (core.CIJob{ID: 5, Name: "tests 1/2", Status: "success", WebURL: "https://g/j/5"}) {
		t.Fatalf("%+v %v", jobs, err)
	}
	if p := f.requests()[0].RawPath; p != "/api/v4/projects/42/pipelines/9/jobs" {
		t.Errorf("path %s", p)
	}
}

func TestGetJobArtifact(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42/jobs/5/artifacts/build/coverage%20report.xml":
			w.Write([]byte("<coverage/>"))
		case "/api/v4/projects/42/jobs/5/artifacts/big.xml":
			w.Write([]byte(strings.Repeat("x", 100)))
		default:
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"message": "404 Not Found"})
		}
	})
	b, err := c.GetJobArtifact(context.Background(), 42, 5, "/build/coverage report.xml", 1000)
	if err != nil || string(b) != "<coverage/>" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := c.GetJobArtifact(context.Background(), 42, 5, "big.xml", 10); !errors.Is(err, ErrTooLarge) {
		t.Errorf("want ErrTooLarge, got %v", err)
	}
	n := len(f.requests())
	if _, err := c.GetJobArtifact(context.Background(), 42, 5, "missing.xml", 10); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if len(f.requests()) != n+1 {
		t.Error("404 must not be retried")
	}
	if _, err := c.GetJobArtifact(context.Background(), 42, 5, "../etc/passwd", 10); err == nil {
		t.Error("path traversal must be refused")
	}
}

// GitLab redirects artifact downloads to object storage: the token must
// not follow the redirect to another host, but must stay on GitLab itself.
func TestRedirectDropsTokenOnOtherHost(t *testing.T) {
	var storageToken, storageAuth string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageToken, storageAuth = r.Header.Get("PRIVATE-TOKEN"), r.Header.Get("Authorization")
		w.Write([]byte("report"))
	}))
	defer storage.Close()
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/1/jobs/2/artifacts/a.xml":
			http.Redirect(w, r, storage.URL+"/bucket/a.xml?X-Amz-Signature=s", http.StatusFound)
		case "/api/v4/projects/1/jobs/3/artifacts/a.xml":
			http.Redirect(w, r, "/api/v4/projects/1/jobs/2/final", http.StatusFound)
		case "/api/v4/projects/1/jobs/2/final":
			w.Write([]byte("same host"))
		}
	})
	b, err := c.GetJobArtifact(context.Background(), 1, 2, "a.xml", 100)
	if err != nil || string(b) != "report" {
		t.Fatalf("%q %v", b, err)
	}
	if storageToken != "" || storageAuth != "" {
		t.Fatalf("token leaked to the storage host: %q %q", storageToken, storageAuth)
	}
	if b, err := c.GetJobArtifact(context.Background(), 1, 3, "a.xml", 100); err != nil || string(b) != "same host" {
		t.Fatalf("same-host redirect: %q %v", b, err)
	}
	reqs := f.requests()
	if last := reqs[len(reqs)-1]; last.Token != testToken {
		t.Errorf("token must stay on same-host redirects: %+v", last)
	}
}

func TestGetJobReport(t *testing.T) {
	const report = `[{"check_name":"r","description":"d","location":{"path":"a.go","lines":{"begin":1}}}]`
	oldGitLab := false
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42/jobs/5":
			writeJSON(w, map[string]any{"id": 5, "artifacts": []map[string]any{
				{"file_type": "archive", "filename": "artifacts.zip", "size": 1000},
				{"file_type": "codequality", "filename": "gl-code-quality-report.json", "size": len(report)},
			}})
		case "/api/v4/projects/42/jobs/6":
			writeJSON(w, map[string]any{"id": 6, "artifacts": []map[string]any{{"file_type": "archive", "filename": "artifacts.zip"}}})
		case "/api/v4/projects/42/jobs/7":
			writeJSON(w, map[string]any{"id": 7, "artifacts": []map[string]any{{"file_type": "codequality", "filename": "cq.json", "size": 1 << 30}}})
		case "/api/v4/projects/42/jobs/5/artifacts":
			if oldGitLab || r.URL.Query().Get("file_type") != "codequality" {
				w.Write([]byte("PK\x03\x04zip archive"))
				return
			}
			w.Write([]byte(report))
		case "/api/v4/projects/42/jobs/5/artifacts/gl-code-quality-report.json":
			if oldGitLab {
				w.WriteHeader(http.StatusNotFound)
				writeJSON(w, map[string]any{"message": "404 Not Found"})
				return
			}
			w.Write([]byte(report))
		default:
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]any{"message": "404 Not Found"})
		}
	})
	ctx := context.Background()
	b, err := c.GetJobReport(ctx, 42, 5, "codequality", 1000)
	if err != nil || string(b) != report {
		t.Fatalf("%q %v", b, err)
	}
	var sawType bool
	for _, r := range f.requests() {
		if r.RawPath == "/api/v4/projects/42/jobs/5/artifacts" && r.Query == "file_type=codequality" {
			sawType = true
		}
	}
	if !sawType {
		t.Errorf("report must be requested by file_type: %+v", f.requests())
	}
	if _, err := c.GetJobReport(ctx, 42, 6, "codequality", 1000); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("job without the report: %v", err)
	}
	if _, err := c.GetJobReport(ctx, 42, 7, "codequality", 1000); !errors.Is(err, ErrTooLarge) {
		t.Errorf("report over the cap: %v", err)
	}
	if _, err := c.GetJobReport(ctx, 42, 5, "../x", 1000); err == nil {
		t.Error("bad type must be refused")
	}

	// GitLab < 19.4 ignores file_type and sends the archive; the report is
	// read from the archive when the job also lists it under paths.
	oldGitLab = true
	if _, err := c.GetJobReport(ctx, 42, 5, "codequality", 1000); !errors.Is(err, ErrReportNeedsArchive) {
		t.Errorf("old GitLab, report not in the archive: %v", err)
	}
}
