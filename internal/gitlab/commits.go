package gitlab

import (
	"context"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

type apiCommit struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Message      string    `json:"message"`
	AuthoredDate time.Time `json:"authored_date"`
}

// ListMRCommits returns all commits of the MR (all pages). GitLab returns
// them newest first.
func (c *Client) ListMRCommits(ctx context.Context, projectID, mrIID int64) ([]core.Commit, error) {
	items, err := list[apiCommit](ctx, c, nil, mrSegs(projectID, mrIID, "commits")...)
	if err != nil {
		return nil, err
	}
	out := make([]core.Commit, 0, len(items))
	for _, it := range items {
		out = append(out, core.Commit{SHA: it.ID, Title: it.Title, Message: it.Message, AuthoredAt: it.AuthoredDate})
	}
	return out, nil
}
