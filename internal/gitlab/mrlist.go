package gitlab

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// MRSummary is one entry of an MR list: what the poller needs to notice
// new MRs, pushes, Draft → Ready, title changes and new comments.
type MRSummary struct {
	ProjectID   int64
	ProjectPath string // group/project, from references.full
	IID         int64
	Title       string
	State       string // opened, closed, merged, locked
	Draft       bool
	SHA         string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type apiMRListItem struct {
	ProjectID      int64     `json:"project_id"`
	IID            int64     `json:"iid"`
	Title          string    `json:"title"`
	State          string    `json:"state"`
	Draft          bool      `json:"draft"`
	WorkInProgress bool      `json:"work_in_progress"`
	SHA            string    `json:"sha"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	References     struct {
		Full string `json:"full"`
	} `json:"references"`
}

// ListUpdatedMRs returns the MRs of a group (subgroups included) updated at
// or after since, oldest update first. An empty group lists every MR the
// token can see. A zero since lists all open MRs (the poller's first run).
// Read-only (GET), allowed for the bot's own Reporter token.
//
// GitLab bumps an MR's updated_at on pushes, title/draft/state changes and
// new comments, so this one call per group finds everything the poller has
// to look at.
func (c *Client) ListUpdatedMRs(ctx context.Context, group string, since time.Time) ([]MRSummary, error) {
	q := url.Values{}
	q.Set("scope", "all")
	q.Set("order_by", "updated_at")
	q.Set("sort", "asc")
	if since.IsZero() {
		q.Set("state", "opened")
	} else {
		q.Set("updated_after", since.UTC().Format(time.RFC3339))
	}
	segs := []string{"merge_requests"}
	if g := strings.Trim(strings.TrimSpace(group), "/"); g != "" {
		segs = []string{"groups", seg(g), "merge_requests"}
	}
	items, err := list[apiMRListItem](ctx, c, q, segs...)
	if err != nil {
		return nil, err
	}
	out := make([]MRSummary, 0, len(items))
	for _, m := range items {
		path := m.References.Full
		if i := strings.LastIndexByte(path, '!'); i >= 0 {
			path = path[:i]
		}
		out = append(out, MRSummary{
			ProjectID: m.ProjectID, ProjectPath: path, IID: m.IID, Title: m.Title, State: m.State,
			Draft: m.Draft || m.WorkInProgress, SHA: m.SHA, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		})
	}
	return out, nil
}

// maxOpenMRsPage caps ListOpenMRs (one page).
const maxOpenMRsPage = 100

// ListOpenMRs returns up to limit open merge requests of a project (full
// path), most recently updated first: one request, no paging. Used by the
// system map to look for related merge requests in other repositories.
// Read-only (GET), allowed for the bot's own Reporter token. DiffRefs,
// pipeline and diverged commits are not filled.
func (c *Client) ListOpenMRs(ctx context.Context, projectPath string, limit int) ([]core.MergeRequest, error) {
	projectPath = strings.Trim(strings.TrimSpace(projectPath), "/")
	if projectPath == "" {
		return nil, errors.New("gitlab: empty project path")
	}
	if limit <= 0 || limit > maxOpenMRsPage {
		limit = maxOpenMRsPage
	}
	q := url.Values{}
	q.Set("state", "opened")
	q.Set("order_by", "updated_at")
	q.Set("sort", "desc")
	q.Set("per_page", strconv.Itoa(limit))
	q.Set("page", "1")
	var items []apiMR
	if err := c.getJSON(ctx, c.endpoint(q, "projects", seg(projectPath), "merge_requests"), &items); err != nil {
		return nil, err
	}
	out := make([]core.MergeRequest, 0, len(items))
	for _, m := range items {
		if len(out) == limit {
			break
		}
		out = append(out, core.MergeRequest{
			ProjectID: m.ProjectID, IID: m.IID, Title: m.Title, Description: m.Description, State: m.State,
			Draft: m.Draft || m.WorkInProgress, SourceBranch: m.SourceBranch, TargetBranch: m.TargetBranch,
			AuthorUsername: m.Author.Username, WebURL: m.WebURL, CreatedAt: m.CreatedAt, DivergedCommits: -1,
			DiffRefs: core.DiffRefs{HeadSHA: m.SHA},
		})
	}
	return out, nil
}
