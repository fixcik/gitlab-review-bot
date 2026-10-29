package review

import (
	"errors"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// ---------------------------------------------------------------------------
// Output validation
// ---------------------------------------------------------------------------

// parseReviewOutput unmarshals and normalizes Claude's review answer.
func parseReviewOutput(s string, maxFindings int) (*output.ReviewOutput, error) {
	var out output.ReviewOutput
	if err := jsonschema.UnmarshalStrict(s, &out); err != nil {
		return nil, err
	}
	finding.NormalizeReview(&out, maxFindings)
	return &out, nil
}

func parseChatOutput(s string) (*output.ChatOutput, error) {
	var out output.ChatOutput
	if err := jsonschema.UnmarshalStrict(s, &out); err != nil {
		return nil, err
	}
	out.Reply = strings.TrimSpace(marker.StripBot(out.Reply))
	if out.Reply == "" {
		return nil, errors.New("empty reply")
	}
	return &out, nil
}
