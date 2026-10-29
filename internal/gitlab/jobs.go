package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

type apiJob struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	WebURL        string `json:"web_url"`
	Stage         string `json:"stage"`
	FailureReason string `json:"failure_reason"`
	AllowFailure  bool   `json:"allow_failure"`
}

// ListPipelineJobs returns the jobs of a pipeline (latest attempt of each,
// all pages). Read-only (GET), allowed for a Reporter.
func (c *Client) ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]core.CIJob, error) {
	items, err := list[apiJob](ctx, c, nil, "projects", id(projectID), "pipelines", id(pipelineID), "jobs")
	if err != nil {
		return nil, err
	}
	out := make([]core.CIJob, 0, len(items))
	for _, j := range items {
		out = append(out, core.CIJob{ID: j.ID, Name: j.Name, Status: j.Status, WebURL: j.WebURL,
			Stage: j.Stage, FailureReason: j.FailureReason, AllowFailure: j.AllowFailure})
	}
	return out, nil
}

// GetJobArtifact downloads one file from a job's artifacts archive
// (GET /projects/:id/jobs/:job_id/artifacts/*artifact_path), at most
// maxBytes (ErrTooLarge beyond). A missing file or expired artifacts is
// core.ErrNotFound. GitLab may redirect to object storage: the token is not
// forwarded there (see checkRedirect).
func (c *Client) GetJobArtifact(ctx context.Context, projectID, jobID int64, artifactPath string, maxBytes int64) ([]byte, error) {
	p := strings.Trim(strings.TrimSpace(artifactPath), "/")
	if p == "" || strings.Contains(p, "..") {
		return nil, errors.New("gitlab: bad artifact path")
	}
	segs := []string{"projects", id(projectID), "jobs", id(jobID), "artifacts"}
	for _, part := range strings.Split(p, "/") {
		segs = append(segs, seg(part))
	}
	resp, err := c.do(ctx, request{
		method:            http.MethodGet,
		u:                 c.endpoint(url.Values{}, segs...),
		retryServerErrors: true,
		maxBytes:          maxBytes,
	})
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}
