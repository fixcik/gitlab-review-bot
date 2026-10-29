package gitlab

import (
	"context"
	"net/http"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Award emoji (reactions) of an MR and of its notes. noteID 0 means the MR
// itself. Needs a client with WriteAwards (BotWrites, set in main); GitLab
// lets any user who can read the MR (Reporter and up) award emoji.

type apiAward struct {
	ID   int64   `json:"id"`
	Name string  `json:"name"`
	User apiUser `json:"user"`
}

type awardReq struct {
	Name string `json:"name"`
}

func awardSegs(projectID, mrIID, noteID int64, more ...string) []string {
	segs := mrSegs(projectID, mrIID)
	if noteID != 0 {
		segs = append(segs, "notes", id(noteID))
	}
	return append(append(segs, "award_emoji"), more...)
}

func toAward(a apiAward) core.Award {
	return core.Award{ID: a.ID, Name: a.Name, UserID: a.User.ID, Username: a.User.Username}
}

// ListAwards returns every award emoji on the MR (noteID 0) or on a note.
func (c *Client) ListAwards(ctx context.Context, projectID, mrIID, noteID int64) ([]core.Award, error) {
	items, err := list[apiAward](ctx, c, nil, awardSegs(projectID, mrIID, noteID)...)
	if err != nil {
		return nil, err
	}
	out := make([]core.Award, 0, len(items))
	for _, a := range items {
		out = append(out, toAward(a))
	}
	return out, nil
}

// AddAward awards emoji name (e.g. "eyes") as the token's user. GitLab
// refuses a duplicate award with a 4xx; callers that need idempotency list
// the awards first (internal/reactions). Retried on 5xx: a request that was
// applied before the error turns into such a duplicate, not a second award.
func (c *Client) AddAward(ctx context.Context, projectID, mrIID, noteID int64, name string) (core.Award, error) {
	resp, err := c.do(ctx, request{
		method:            http.MethodPost,
		u:                 c.endpoint(nil, awardSegs(projectID, mrIID, noteID)...),
		body:              awardReq{Name: name},
		retryServerErrors: true,
	})
	if err != nil {
		return core.Award{}, err
	}
	var a apiAward
	if err := decode(resp.body, &a); err != nil {
		return core.Award{}, err
	}
	return toAward(a), nil
}

// DeleteAward removes one award by id (GitLab allows removing only the
// token user's own awards).
func (c *Client) DeleteAward(ctx context.Context, projectID, mrIID, noteID, awardID int64) error {
	_, err := c.do(ctx, request{
		method:            http.MethodDelete,
		u:                 c.endpoint(nil, awardSegs(projectID, mrIID, noteID, id(awardID))...),
		retryServerErrors: true,
	})
	return err
}
