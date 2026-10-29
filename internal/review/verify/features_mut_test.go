package verify

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// A verification answer must carry at least one verdict for an existing
// item; verdicts are normalized.
func TestParseCompatVerificationMut(t *testing.T) {
	if _, err := parseCompatVerification(`{"verdicts":[{"index":3,"verdict":"confirmed","reason":"r"},{"index":0,"verdict":"rejected","reason":"r"}]}`, 2); err == nil {
		t.Error("verdicts only for unknown items must be refused")
	}
	if _, err := parseCompatVerification(`{"verdicts":[]}`, 1); err == nil {
		t.Error("no verdicts for one item must be refused")
	}
	if _, err := parseCompatVerification(`{"verdicts":[]}`, 0); err != nil {
		t.Errorf("nothing to verify: %v", err)
	}
	v, err := parseCompatVerification(`{"verdicts":[{"index":2,"verdict":" Rejected ","reason":" r "},{"index":5,"verdict":"confirmed","reason":"x"}]}`, 2)
	if err != nil || v.Verdicts[0].Verdict != output.VerdictRejected || v.Verdicts[0].Reason != "r" {
		t.Errorf("%+v %v", v, err)
	}
}
