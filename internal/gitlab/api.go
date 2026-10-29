package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type apiUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type apiDiffRefs struct {
	BaseSHA  string `json:"base_sha"`
	StartSHA string `json:"start_sha"`
	HeadSHA  string `json:"head_sha"`
}

type apiMR struct {
	ProjectID      int64        `json:"project_id"`
	IID            int64        `json:"iid"`
	Title          string       `json:"title"`
	Description    string       `json:"description"`
	State          string       `json:"state"`
	Draft          bool         `json:"draft"`
	WorkInProgress bool         `json:"work_in_progress"`
	SourceBranch   string       `json:"source_branch"`
	TargetBranch   string       `json:"target_branch"`
	Author         apiUser      `json:"author"`
	WebURL         string       `json:"web_url"`
	DiffRefs       *apiDiffRefs `json:"diff_refs"`
	SHA            string       `json:"sha"`
	CreatedAt      time.Time    `json:"created_at"`
	// DivergedCommitsCount is returned with include_diverged_commits_count.
	DivergedCommitsCount *int `json:"diverged_commits_count"`
	HeadPipeline         *struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		SHA    string `json:"sha"`
		WebURL string `json:"web_url"`
	} `json:"head_pipeline"`
}

type apiDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	Diff        string `json:"diff"`
}

type apiPosition struct {
	BaseSHA  string `json:"base_sha"`
	StartSHA string `json:"start_sha"`
	HeadSHA  string `json:"head_sha"`
	OldPath  string `json:"old_path"`
	NewPath  string `json:"new_path"`
	OldLine  *int   `json:"old_line"`
	NewLine  *int   `json:"new_line"`
}

type apiNote struct {
	ID         int64        `json:"id"`
	Body       string       `json:"body"`
	Author     apiUser      `json:"author"`
	System     bool         `json:"system"`
	CreatedAt  time.Time    `json:"created_at"`
	Position   *apiPosition `json:"position"`
	Resolvable bool         `json:"resolvable"`
	Resolved   bool         `json:"resolved"`
	ResolvedBy *apiUser     `json:"resolved_by"`
}

type apiDiscussion struct {
	ID             string    `json:"id"`
	IndividualNote bool      `json:"individual_note"`
	Notes          []apiNote `json:"notes"`
}

// positionReq is the request form of a text position. Zero lines are
// omitted: GitLab treats an explicit null/0 differently from "not set".
type positionReq struct {
	PositionType string `json:"position_type"`
	BaseSHA      string `json:"base_sha"`
	StartSHA     string `json:"start_sha"`
	HeadSHA      string `json:"head_sha"`
	OldPath      string `json:"old_path,omitempty"`
	NewPath      string `json:"new_path,omitempty"`
	OldLine      int    `json:"old_line,omitempty"`
	NewLine      int    `json:"new_line,omitempty"`
}

type draftNoteReq struct {
	Note     string       `json:"note"`
	Position *positionReq `json:"position,omitempty"`
}

type bodyReq struct {
	Body string `json:"body"`
}

type idResp struct {
	ID int64 `json:"id"`
}

// decodeID is the id of the object a POST created.
func decodeID(resp *response) (int64, error) {
	var r idResp
	if err := decode(resp.body, &r); err != nil {
		return 0, err
	}
	return r.ID, nil
}

// ---------------------------------------------------------------------------
// Conversions
// ---------------------------------------------------------------------------

func toNote(n apiNote) core.Note {
	out := core.Note{
		ID:             n.ID,
		AuthorID:       n.Author.ID,
		AuthorUsername: n.Author.Username,
		Body:           n.Body,
		System:         n.System,
		CreatedAt:      n.CreatedAt,
	}
	if n.ResolvedBy != nil {
		out.ResolvedByID, out.ResolvedByUsername = n.ResolvedBy.ID, n.ResolvedBy.Username
	}
	if p := n.Position; p != nil {
		pos := &core.Position{
			BaseSHA:  p.BaseSHA,
			StartSHA: p.StartSHA,
			HeadSHA:  p.HeadSHA,
			OldPath:  p.OldPath,
			NewPath:  p.NewPath,
		}
		if p.OldLine != nil {
			pos.OldLine = *p.OldLine
		}
		if p.NewLine != nil {
			pos.NewLine = *p.NewLine
		}
		out.Position = pos
	}
	return out
}

func toDiscussion(d apiDiscussion) core.Discussion {
	out := core.Discussion{ID: d.ID, Individual: d.IndividualNote, Notes: make([]core.Note, 0, len(d.Notes))}
	// A discussion is resolvable/resolved when its resolvable notes are.
	resolvedAll, anyResolvable := true, false
	for _, n := range d.Notes {
		out.Notes = append(out.Notes, toNote(n))
		if n.Resolvable {
			anyResolvable = true
			if !n.Resolved {
				resolvedAll = false
			}
		}
	}
	out.Resolvable = anyResolvable
	out.Resolved = anyResolvable && resolvedAll
	return out
}

func toPositionReq(p *core.Position) *positionReq {
	if p == nil {
		return nil
	}
	return &positionReq{
		PositionType: "text",
		BaseSHA:      p.BaseSHA,
		StartSHA:     p.StartSHA,
		HeadSHA:      p.HeadSHA,
		OldPath:      p.OldPath,
		NewPath:      p.NewPath,
		OldLine:      p.OldLine,
		NewLine:      p.NewLine,
	}
}

func mrSegs(projectID, mrIID int64, more ...string) []string {
	return append([]string{"projects", id(projectID), "merge_requests", id(mrIID)}, more...)
}

// ---------------------------------------------------------------------------
// core.GitLab
// ---------------------------------------------------------------------------

// CurrentUser returns the authenticated user.
func (c *Client) CurrentUser(ctx context.Context) (int64, string, error) {
	var u apiUser
	if err := c.getJSON(ctx, c.endpoint(nil, "user"), &u); err != nil {
		return 0, "", err
	}
	return u.ID, u.Username, nil
}

// GetMR returns one merge request.
func (c *Client) GetMR(ctx context.Context, projectID, mrIID int64) (*core.MergeRequest, error) {
	var m apiMR
	q := url.Values{}
	q.Set("include_diverged_commits_count", "true")
	if err := c.getJSON(ctx, c.endpoint(q, mrSegs(projectID, mrIID)...), &m); err != nil {
		return nil, err
	}
	out := &core.MergeRequest{
		ProjectID:       m.ProjectID,
		IID:             m.IID,
		Title:           m.Title,
		Description:     m.Description,
		State:           m.State,
		Draft:           m.Draft || m.WorkInProgress,
		SourceBranch:    m.SourceBranch,
		TargetBranch:    m.TargetBranch,
		AuthorUsername:  m.Author.Username,
		WebURL:          m.WebURL,
		CreatedAt:       m.CreatedAt,
		DivergedCommits: -1,
	}
	if m.DivergedCommitsCount != nil {
		out.DivergedCommits = *m.DivergedCommitsCount
	}
	if p := m.HeadPipeline; p != nil && p.Status != "" {
		out.Pipeline = &core.Pipeline{ID: p.ID, Status: p.Status, SHA: p.SHA, WebURL: p.WebURL}
	}
	if out.ProjectID == 0 {
		out.ProjectID = projectID
	}
	if out.IID == 0 {
		out.IID = mrIID
	}
	if m.DiffRefs != nil {
		out.DiffRefs = core.DiffRefs{BaseSHA: m.DiffRefs.BaseSHA, StartSHA: m.DiffRefs.StartSHA, HeadSHA: m.DiffRefs.HeadSHA}
	}
	if out.DiffRefs.HeadSHA == "" {
		out.DiffRefs.HeadSHA = m.SHA
	}
	return out, nil
}

// GetMRDiffs returns all file diffs of the MR.
func (c *Client) GetMRDiffs(ctx context.Context, projectID, mrIID int64) ([]core.FileDiff, error) {
	items, err := list[apiDiff](ctx, c, nil, mrSegs(projectID, mrIID, "diffs")...)
	if err != nil {
		return nil, err
	}
	out := make([]core.FileDiff, 0, len(items))
	for _, d := range items {
		out = append(out, core.FileDiff{
			OldPath:     d.OldPath,
			NewPath:     d.NewPath,
			NewFile:     d.NewFile,
			RenamedFile: d.RenamedFile,
			DeletedFile: d.DeletedFile,
			Diff:        d.Diff,
		})
	}
	return out, nil
}

// ListDiscussions returns all discussions of the MR.
func (c *Client) ListDiscussions(ctx context.Context, projectID, mrIID int64) ([]core.Discussion, error) {
	items, err := list[apiDiscussion](ctx, c, nil, mrSegs(projectID, mrIID, "discussions")...)
	if err != nil {
		return nil, err
	}
	out := make([]core.Discussion, 0, len(items))
	for _, d := range items {
		out = append(out, toDiscussion(d))
	}
	return out, nil
}

// GetDiscussion returns one discussion.
func (c *Client) GetDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string) (*core.Discussion, error) {
	var d apiDiscussion
	if err := c.getJSON(ctx, c.endpoint(nil, mrSegs(projectID, mrIID, "discussions", seg(discussionID))...), &d); err != nil {
		return nil, err
	}
	out := toDiscussion(d)
	return &out, nil
}

// CreateDraftNote creates an unpublished note, optionally anchored to pos.
// Not retried on 5xx (it could create a duplicate).
func (c *Client) CreateDraftNote(ctx context.Context, projectID, mrIID int64, body string, pos *core.Position) (int64, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodPost,
		u:      c.endpoint(nil, mrSegs(projectID, mrIID, "draft_notes")...),
		body:   draftNoteReq{Note: body, Position: toPositionReq(pos)},
	})
	if err != nil {
		return 0, err
	}
	return decodeID(resp)
}

// PublishDraftNotes publishes all of the current user's drafts on the MR.
// Publishing is idempotent (already published drafts are gone), so server
// errors are retried.
func (c *Client) PublishDraftNotes(ctx context.Context, projectID, mrIID int64) error {
	_, err := c.do(ctx, request{
		method:            http.MethodPost,
		u:                 c.endpoint(nil, mrSegs(projectID, mrIID, "draft_notes", "bulk_publish")...),
		retryServerErrors: true,
	})
	return err
}

// DeleteDraftNotes deletes all of the current user's drafts on the MR.
func (c *Client) DeleteDraftNotes(ctx context.Context, projectID, mrIID int64) error {
	drafts, err := list[idResp](ctx, c, nil, mrSegs(projectID, mrIID, "draft_notes")...)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range drafts {
		_, err := c.do(ctx, request{
			method:            http.MethodDelete,
			u:                 c.endpoint(nil, mrSegs(projectID, mrIID, "draft_notes", id(d.ID))...),
			retryServerErrors: true,
		})
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, fmt.Errorf("delete draft note %d: %w", d.ID, err))
		}
	}
	return errors.Join(errs...)
}

// CreateNote adds a top-level MR note. Not retried on 5xx.
func (c *Client) CreateNote(ctx context.Context, projectID, mrIID int64, body string) (int64, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodPost,
		u:      c.endpoint(nil, mrSegs(projectID, mrIID, "notes")...),
		body:   bodyReq{Body: body},
	})
	if err != nil {
		return 0, err
	}
	return decodeID(resp)
}

// UpdateNote replaces the body of an MR note.
func (c *Client) UpdateNote(ctx context.Context, projectID, mrIID, noteID int64, body string) error {
	_, err := c.do(ctx, request{
		method:            http.MethodPut,
		u:                 c.endpoint(nil, mrSegs(projectID, mrIID, "notes", id(noteID))...),
		body:              bodyReq{Body: body},
		retryServerErrors: true,
	})
	return err
}

// ReplyToDiscussion adds a note to a discussion. Not retried on 5xx.
func (c *Client) ReplyToDiscussion(ctx context.Context, projectID, mrIID int64, discussionID, body string) error {
	_, err := c.do(ctx, request{
		method: http.MethodPost,
		u:      c.endpoint(nil, mrSegs(projectID, mrIID, "discussions", seg(discussionID), "notes")...),
		body:   bodyReq{Body: body},
	})
	return err
}

// ResolveDiscussion resolves or unresolves a discussion.
func (c *Client) ResolveDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string, resolved bool) error {
	_, err := c.do(ctx, request{
		method:            http.MethodPut,
		u:                 c.endpoint(nil, mrSegs(projectID, mrIID, "discussions", seg(discussionID))...),
		body:              resolveUpdate{Resolved: resolved},
		retryServerErrors: true,
	})
	return err
}

// AccessLevel returns the user's effective access level in the project
// (including inherited membership); 0 if the user is not a member.
func (c *Client) AccessLevel(ctx context.Context, projectID, userID int64) (int, error) {
	var m struct {
		AccessLevel int `json:"access_level"`
	}
	err := c.getJSON(ctx, c.endpoint(nil, "projects", id(projectID), "members", "all", id(userID)), &m)
	if errors.Is(err, core.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return m.AccessLevel, nil
}

// GetRawFile returns the raw content of path at ref.
func (c *Client) GetRawFile(ctx context.Context, projectID int64, path, ref string) ([]byte, error) {
	return c.GetRawFileMax(ctx, projectID, path, ref, 0)
}

// GetRawFileMax is GetRawFile with a size cap: a file over max bytes is not
// read past the cap and fails with ErrTooLarge (max 0 = no cap).
func (c *Client) GetRawFileMax(ctx context.Context, projectID int64, path, ref string, max int64) ([]byte, error) {
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	resp, err := c.do(ctx, request{
		method:            http.MethodGet,
		u:                 c.endpoint(q, "projects", id(projectID), "repository", "files", seg(path), "raw"),
		retryServerErrors: true,
		maxBytes:          max,
	})
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

// resolveUpdate is the body of a discussion (un)resolve request.
type resolveUpdate struct {
	Resolved bool `json:"resolved"`
}

// reviewersUpdate is the only MR update the bot ever sends.
type reviewersUpdate struct {
	ReviewerIDs []int64 `json:"reviewer_ids"`
}

// AddReviewer adds userID to the MR's reviewers, keeping the existing ones.
// It reports whether the list changed. Needs a client with WriteReviewers
// and a token allowed to edit the MR (Developer+).
func (c *Client) AddReviewer(ctx context.Context, projectID, mrIID, userID int64) (bool, error) {
	var mr struct {
		Reviewers []apiUser `json:"reviewers"`
	}
	if err := c.getJSON(ctx, c.endpoint(nil, mrSegs(projectID, mrIID)...), &mr); err != nil {
		return false, err
	}
	ids := make([]int64, 0, len(mr.Reviewers)+1)
	for _, u := range mr.Reviewers {
		if u.ID == userID {
			return false, nil
		}
		ids = append(ids, u.ID)
	}
	ids = append(ids, userID)
	_, err := c.do(ctx, request{
		method:            http.MethodPut,
		u:                 c.endpoint(nil, mrSegs(projectID, mrIID)...),
		body:              reviewersUpdate{ReviewerIDs: ids},
		retryServerErrors: true,
	})
	return err == nil, err
}
