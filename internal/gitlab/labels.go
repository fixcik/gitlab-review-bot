package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// labelsUpdate is the body of an MR labels update: only add_labels and
// remove_labels (comma-separated names), never anything else about the MR.
type labelsUpdate struct {
	AddLabels    string `json:"add_labels,omitempty"`
	RemoveLabels string `json:"remove_labels,omitempty"`
}

// mrLabels is the part of an MR the label methods read.
type mrLabels struct {
	State  string   `json:"state"`
	Labels []string `json:"labels"`
}

// MRLabels returns the MR's state (opened, closed, merged, locked) and its
// current labels.
func (c *Client) MRLabels(ctx context.Context, projectID, mrIID int64) (string, []string, error) {
	var m mrLabels
	if err := c.getJSON(ctx, c.endpoint(nil, mrSegs(projectID, mrIID)...), &m); err != nil {
		return "", nil, err
	}
	return m.State, m.Labels, nil
}

// UpdateMRLabels adds and removes MR labels in one request
// (PUT /projects/:id/merge_requests/:iid with add_labels / remove_labels)
// and returns the MR's labels after the update. Names must not contain
// commas. GitLab creates a missing added label when the token's user may
// create labels; a user without the right to change the MR's metadata gets
// 403. Needs a client with WriteLabels.
func (c *Client) UpdateMRLabels(ctx context.Context, projectID, mrIID int64, add, remove []string) ([]string, error) {
	for _, l := range append(append([]string(nil), add...), remove...) {
		if strings.Contains(l, ",") || strings.TrimSpace(l) == "" {
			return nil, errors.New("gitlab: invalid label name")
		}
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil, errors.New("gitlab: no label changes")
	}
	resp, err := c.do(ctx, request{
		method:            http.MethodPut,
		u:                 c.endpoint(nil, mrSegs(projectID, mrIID)...),
		body:              labelsUpdate{AddLabels: strings.Join(add, ","), RemoveLabels: strings.Join(remove, ",")},
		retryServerErrors: true, // idempotent: adding/removing the same labels again changes nothing
	})
	if err != nil {
		return nil, err
	}
	var m mrLabels
	if err := decode(resp.body, &m); err != nil {
		return nil, err
	}
	return m.Labels, nil
}

// ListProjectLabels returns the names of the labels available in the
// project: its own and those of its ancestor groups
// (GET /projects/:id/labels?include_ancestor_groups=true).
func (c *Client) ListProjectLabels(ctx context.Context, projectID int64) ([]string, error) {
	q := url.Values{}
	q.Set("include_ancestor_groups", "true")
	q.Set("with_counts", "false")
	items, err := list[struct {
		Name string `json:"name"`
	}](ctx, c, q, "projects", id(projectID), "labels")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, l := range items {
		out = append(out, l.Name)
	}
	return out, nil
}
