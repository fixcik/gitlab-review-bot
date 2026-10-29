package gitlab

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// ProjectDetails is a group project with what the context-repository
// feature needs: the default branch and, when GitLab returns it, the
// repository size.
type ProjectDetails struct {
	ID                int64
	PathWithNamespace string
	DefaultBranch     string // empty for an empty repository
	Archived          bool
	// RepositorySize is statistics.repository_size in bytes; 0 when GitLab
	// did not return statistics.
	RepositorySize int64
}

type apiProjectDetails struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	DefaultBranch     string `json:"default_branch"`
	Archived          bool   `json:"archived"`
	Statistics        *struct {
		RepositorySize int64 `json:"repository_size"`
	} `json:"statistics"`
}

func (p apiProjectDetails) details() ProjectDetails {
	d := ProjectDetails{ID: p.ID, PathWithNamespace: p.PathWithNamespace, DefaultBranch: p.DefaultBranch, Archived: p.Archived}
	if p.Statistics != nil {
		d.RepositorySize = p.Statistics.RepositorySize
	}
	return d
}

// ListGroupProjectsDetails returns all non-archived projects of the group
// (subgroups included, shared projects excluded) with their default branch
// and repository size. statistics=true is honoured for Reporter and above;
// without it RepositorySize stays 0. Read-only (GET), allowed for the bot's
// own Reporter token.
func (c *Client) ListGroupProjectsDetails(ctx context.Context, groupPath string) ([]ProjectDetails, error) {
	groupPath = strings.Trim(strings.TrimSpace(groupPath), "/")
	if groupPath == "" {
		return nil, errors.New("gitlab: empty group path")
	}
	q := url.Values{}
	q.Set("include_subgroups", "true")
	q.Set("archived", "false")
	q.Set("with_shared", "false")
	q.Set("statistics", "true")
	q.Set("simple", "false")
	items, err := list[apiProjectDetails](ctx, c, q, "groups", seg(groupPath), "projects")
	if err != nil {
		return nil, err
	}
	out := make([]ProjectDetails, 0, len(items))
	for _, p := range items {
		out = append(out, p.details())
	}
	return out, nil
}

// GetProjectDetails returns one project by its full path, with the default
// branch and (for Reporter and above) the repository size. Used for context
// repositories named in context.related that lie outside gitlab.groups.
// Read-only (GET). A project the token cannot see is core.ErrNotFound.
func (c *Client) GetProjectDetails(ctx context.Context, path string) (*ProjectDetails, error) {
	path = strings.Trim(strings.TrimSpace(path), "/")
	if path == "" {
		return nil, errors.New("gitlab: empty project path")
	}
	q := url.Values{}
	q.Set("statistics", "true")
	var p apiProjectDetails
	if err := c.getJSON(ctx, c.endpoint(q, "projects", seg(path)), &p); err != nil {
		return nil, err
	}
	d := p.details()
	return &d, nil
}
