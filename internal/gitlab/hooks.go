package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// This file holds the group/project-hook endpoints used by hook sync
// (internal/hooksync). They are methods on *Client only and are not part of
// core.GitLab.

// Project is a project listed in a group.
type Project struct {
	ID                int64
	PathWithNamespace string
	Archived          bool
}

// Hook is a project webhook as GitLab returns it. GitLab never returns the
// secret token, so there is no Token field.
type Hook struct {
	ID                    int64
	URL                   string
	MergeRequestsEvents   bool
	NoteEvents            bool
	PushEvents            bool
	PipelineEvents        bool
	EnableSSLVerification bool
}

// HookSpec is the desired state of a project webhook. Token is the secret
// GitLab sends in X-Gitlab-Token; it is never logged or returned.
type HookSpec struct {
	URL                   string
	Token                 string
	MergeRequestsEvents   bool
	NoteEvents            bool
	PipelineEvents        bool
	EnableSSLVerification bool
}

type apiProject struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	Archived          bool   `json:"archived"`
}

type apiHook struct {
	ID                    int64  `json:"id"`
	URL                   string `json:"url"`
	MergeRequestsEvents   bool   `json:"merge_requests_events"`
	NoteEvents            bool   `json:"note_events"`
	PushEvents            bool   `json:"push_events"`
	PipelineEvents        bool   `json:"pipeline_events"`
	EnableSSLVerification bool   `json:"enable_ssl_verification"`
}

// hookReq is the create/edit payload. push_events is always sent explicitly:
// GitLab enables push events by default on create, and the bot has no use
// for them.
type hookReq struct {
	URL                   string `json:"url"`
	Token                 string `json:"token"`
	MergeRequestsEvents   bool   `json:"merge_requests_events"`
	NoteEvents            bool   `json:"note_events"`
	PushEvents            bool   `json:"push_events"`
	PipelineEvents        bool   `json:"pipeline_events"`
	EnableSSLVerification bool   `json:"enable_ssl_verification"`
}

func (s HookSpec) req() hookReq {
	return hookReq{
		URL:                   s.URL,
		Token:                 s.Token,
		MergeRequestsEvents:   s.MergeRequestsEvents,
		NoteEvents:            s.NoteEvents,
		PushEvents:            false,
		PipelineEvents:        s.PipelineEvents,
		EnableSSLVerification: s.EnableSSLVerification,
	}
}

// ListGroupProjects returns all non-archived projects of the group, including
// subgroups (shared projects excluded). groupPath is the full path, e.g.
// "platform/services".
func (c *Client) ListGroupProjects(ctx context.Context, groupPath string) ([]Project, error) {
	groupPath = strings.Trim(strings.TrimSpace(groupPath), "/")
	if groupPath == "" {
		return nil, errors.New("gitlab: empty group path")
	}
	q := url.Values{}
	q.Set("include_subgroups", "true")
	q.Set("archived", "false")
	q.Set("with_shared", "false")
	items, err := list[apiProject](ctx, c, q, "groups", seg(groupPath), "projects")
	if err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(items))
	for _, p := range items {
		out = append(out, Project(p))
	}
	return out, nil
}

// ListProjectHooks returns all webhooks of the project (requires Maintainer).
func (c *Client) ListProjectHooks(ctx context.Context, projectID int64) ([]Hook, error) {
	items, err := list[apiHook](ctx, c, nil, "projects", id(projectID), "hooks")
	if err != nil {
		return nil, err
	}
	out := make([]Hook, 0, len(items))
	for _, h := range items {
		out = append(out, Hook(h))
	}
	return out, nil
}

// AddProjectHook creates a project webhook and returns its ID. Not retried on
// 5xx (it could create a duplicate hook).
func (c *Client) AddProjectHook(ctx context.Context, projectID int64, s HookSpec) (int64, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodPost,
		u:      c.endpoint(nil, "projects", id(projectID), "hooks"),
		body:   s.req(),
	})
	if err != nil {
		return 0, hideSecret(err, s.Token)
	}
	return decodeID(resp)
}

// EditProjectHook overwrites a project webhook with s (including the token).
func (c *Client) EditProjectHook(ctx context.Context, projectID, hookID int64, s HookSpec) error {
	_, err := c.do(ctx, request{
		method:            http.MethodPut,
		u:                 c.endpoint(nil, "projects", id(projectID), "hooks", id(hookID)),
		body:              s.req(),
		retryServerErrors: true,
	})
	return hideSecret(err, s.Token)
}

// hideSecret removes the hook token from an error (e.g. if GitLab echoes the
// request in a validation message). Unlike redact it does not skip short
// values: the hook token is never expected in error text at all.
func hideSecret(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	var ge *Error
	if errors.As(err, &ge) {
		if strings.Contains(ge.Body, secret) {
			cp := *ge
			cp.Body = strings.ReplaceAll(cp.Body, secret, "[REDACTED]")
			return &cp
		}
		return err
	}
	if strings.Contains(err.Error(), secret) {
		return scrubbed{strings.ReplaceAll(err.Error(), secret, "[REDACTED]")}
	}
	return err
}
