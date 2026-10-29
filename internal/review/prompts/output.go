package prompts

import (
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

// CleanModelText: the model's text is untrusted (prompt injection from the
// MR): no bot markers, no quick actions; problems.CleanText strips markup.
func CleanModelText(s string) string {
	return mdtext.NeutralizeQuickActions(marker.StripBot(s))
}
