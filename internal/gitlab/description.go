package gitlab

import (
	"context"
	"errors"
	"net/http"
)

// MaxDescriptionBytes is GitLab's limit of an MR description (1 MB).
const MaxDescriptionBytes = 1_000_000

// descriptionUpdate is the body of an MR description update: only the
// description, never anything else about the MR.
type descriptionUpdate struct {
	Description string `json:"description"`
}

// UpdateMRDescription replaces the MR's description
// (PUT /projects/:id/merge_requests/:iid with description only). The caller
// reads the description right before and changes only its own block. A
// user without the right to edit the MR gets 403. Not retried on server
// errors: the text may be saved already, and the next review tries again.
// Needs a client with WriteDescription.
func (c *Client) UpdateMRDescription(ctx context.Context, projectID, mrIID int64, description string) error {
	if len(description) > MaxDescriptionBytes {
		return errors.New("gitlab: description over 1 MB")
	}
	_, err := c.do(ctx, request{
		method: http.MethodPut,
		u:      c.endpoint(nil, mrSegs(projectID, mrIID)...),
		body:   descriptionUpdate{Description: description},
	})
	return err
}
