package gitlab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Report artifacts of a job (artifacts:reports:<type>), used by the CI
// linters of lint.ci (report: codequality).

// ErrReportNeedsArchive is core.ErrReportNeedsArchive (callers match the
// core sentinel).
var ErrReportNeedsArchive = core.ErrReportNeedsArchive

type apiJobArtifacts struct {
	Artifacts []struct {
		FileType string `json:"file_type"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	} `json:"artifacts"`
}

// GetJobReport downloads the report artifact of type fileType (e.g.
// "codequality") of a job, at most maxBytes. A job without such a report
// (or with expired artifacts) is core.ErrNotFound.
//
// GET /projects/:id/jobs/:job_id lists the job's artifacts with their
// file_type; GET /projects/:id/jobs/:job_id/artifacts?file_type=<type>
// downloads one report (GitLab 19.4+, lib/api/ci/job_artifacts.rb). Older
// servers ignore file_type and send the zip archive: then the report's
// filename is looked up inside the archive (it is there when the job also
// lists it under artifacts:paths), else ErrReportNeedsArchive.
func (c *Client) GetJobReport(ctx context.Context, projectID, jobID int64, fileType string, maxBytes int64) ([]byte, error) {
	fileType = strings.TrimSpace(fileType)
	if fileType == "" || fileType == "archive" || strings.ContainsAny(fileType, "/?&#") {
		return nil, errors.New("gitlab: bad report type")
	}
	var job apiJobArtifacts
	if err := c.getJSON(ctx, c.endpoint(nil, "projects", id(projectID), "jobs", id(jobID)), &job); err != nil {
		return nil, err
	}
	filename := ""
	found := false
	for _, a := range job.Artifacts {
		if a.FileType != fileType {
			continue
		}
		if maxBytes > 0 && a.Size > maxBytes {
			return nil, fmt.Errorf("%w: %s report of job %d is %d bytes", ErrTooLarge, fileType, jobID, a.Size)
		}
		found, filename = true, a.Filename
		break
	}
	if !found {
		return nil, fmt.Errorf("%w: job %d has no %s report", core.ErrNotFound, jobID, fileType)
	}
	resp, err := c.do(ctx, request{
		method:            http.MethodGet,
		u:                 c.endpoint(url.Values{"file_type": {fileType}}, "projects", id(projectID), "jobs", id(jobID), "artifacts"),
		retryServerErrors: true,
		maxBytes:          maxBytes,
	})
	switch {
	case err == nil && !bytes.HasPrefix(resp.body, []byte("PK\x03\x04")):
		return resp.body, nil
	case err != nil && !errors.Is(err, ErrTooLarge):
		return nil, err
	}
	// The archive came back (an old GitLab ignoring file_type; the report
	// itself fits maxBytes, see above): the report may be in it under its
	// file name.
	if filename == "" || strings.Contains(filename, "..") {
		return nil, ErrReportNeedsArchive
	}
	b, err := c.GetJobArtifact(ctx, projectID, jobID, strings.TrimSuffix(filename, ".gz"), maxBytes)
	if errors.Is(err, core.ErrNotFound) && strings.HasSuffix(filename, ".gz") {
		b, err = c.GetJobArtifact(ctx, projectID, jobID, filename, maxBytes)
	}
	if errors.Is(err, core.ErrNotFound) {
		return nil, ErrReportNeedsArchive
	}
	return b, err
}
