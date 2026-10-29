package crossrepo

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/citelink"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// Citations in the backward-compatibility part of a review become links as
// well: those threads name the consumers in other repositories.
func TestLinkReviewCoversCompatibility(t *testing.T) {
	l := citelink.New("http://gitlab:8929")
	l.Add("g/frontend", "0123456789abcdef0123456789abcdef01234567")
	rc := &RunContext{links: l}
	cite := "`g/frontend:src/UserCard.ts:14`"
	o := &output.ReviewOutput{Compatibility: &output.Compatibility{
		Breaking: []output.BreakingChange{{
			Title: "t " + cite, Explanation: "e " + cite, PreviousBehavior: "p " + cite,
			NewBehavior: "n " + cite, SafeAlternative: "s " + cite,
		}},
		Resolved: []output.ResolvedFinding{{Comment: "c " + cite}},
	}}
	rc.LinkReview(o)
	b := o.Compatibility.Breaking[0]
	for name, got := range map[string]string{
		"title": b.Title, "explanation": b.Explanation, "previous": b.PreviousBehavior,
		"new": b.NewBehavior, "alternative": b.SafeAlternative, "resolved": o.Compatibility.Resolved[0].Comment,
	} {
		if !strings.Contains(got, "(http://gitlab:8929/g/frontend/-/blob/0123456789abcdef0123456789abcdef01234567/src/UserCard.ts#L14)") {
			t.Errorf("%s not linked: %q", name, got)
		}
	}
}
