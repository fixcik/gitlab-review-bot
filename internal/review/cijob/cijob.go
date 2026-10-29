// Package cijob holds what the CI features share about pipelines and their
// jobs: the artifacts API, parallel job names, job patterns and pipeline
// links. It is a leaf: it must not depend on internal/review or any of its
// feature packages.
package cijob

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

// Artifacts is implemented by the GitLab client (*gitlab.Client); the
// feature is off when the client does not implement it.
type Artifacts interface {
	ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]core.CIJob, error)
	GetJobArtifact(ctx context.Context, projectID, jobID int64, path string, maxBytes int64) ([]byte, error)
}

// IsParallelOf reports whether jobName is a parallel instance of name
// ("phpstan 1/2" of "phpstan").
func IsParallelOf(jobName, name string) bool {
	rest, ok := strings.CutPrefix(jobName, name+" ")
	if !ok {
		return false
	}
	a, b, ok := strings.Cut(rest, "/")
	if !ok {
		return false
	}
	_, e1 := strconv.Atoi(a)
	_, e2 := strconv.Atoi(b)
	return e1 == nil && e2 == nil
}

// Match finds the jobs named (or matching the glob) pattern, parallel
// instances ("phpstan 1/2") included. Any status.
func Match(jobs []core.CIJob, pattern string) []core.CIJob {
	var out []core.CIJob
	for _, j := range jobs {
		base := j.Name
		if i := strings.LastIndexByte(j.Name, ' '); i > 0 && IsParallelOf(j.Name, j.Name[:i]) {
			base = j.Name[:i]
		}
		ok := j.Name == pattern || base == pattern
		if !ok {
			m1, _ := path.Match(pattern, j.Name)
			m2, _ := path.Match(pattern, base)
			ok = m1 || m2
		}
		if ok {
			out = append(out, j)
		}
	}
	return out
}

// PipelineLink is "#N" linked to the pipeline.
func PipelineLink(p core.Pipeline) string {
	s := fmt.Sprintf("#%d", p.ID)
	if mdtext.IsHTTPURL(p.WebURL) {
		return "[" + s + "](" + p.WebURL + ")"
	}
	return s
}

// Running reports a pipeline that has not finished yet.
func Running(p *core.Pipeline) bool {
	if p == nil {
		return false
	}
	switch p.Status {
	case "created", "waiting_for_resource", "preparing", "pending", "running", "scheduled":
		return true
	}
	return false
}
