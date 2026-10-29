package labels

import (
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

// labelOutcome is what happened, for the summary.
type labelOutcome struct {
	added, removed []string
	failed         []string // labels that could not be added or removed
	current        []string // bot labels on the MR afterwards
	held           []string
}

// maxSummaryLabels caps the labels listed in the summary line.
const maxSummaryLabels = 12

// summaryLine: "🏷 Метки: +`a` −`b`" when something changed or failed,
// else the bot's labels on the MR; "" when the bot has none.
func (o labelOutcome) summaryLine() string {
	if len(o.added)+len(o.removed) == 0 && len(o.current) > 0 && len(o.failed) == 0 {
		return "🏷 Метки бота: " + strings.Join(summaryLabels("", o.current), ", ") + "\n\n"
	}
	parts := append(summaryLabels("+", o.added), summaryLabels("−", o.removed)...)
	if len(o.failed) > 0 {
		parts = append(parts, "не удалось: "+strings.Join(summaryLabels("", o.failed), ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "🏷 Метки: " + strings.Join(parts, " ") + "\n\n"
}

// summaryLabels renders names as prefix+`name`, at most maxSummaryLabels
// of them plus "и ещё N".
func summaryLabels(prefix string, names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, prefix+mdtext.InlineCode(n))
	}
	return mdtext.CapList(out, maxSummaryLabels)
}
