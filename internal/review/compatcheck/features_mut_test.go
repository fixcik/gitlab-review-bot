package compatcheck

import (
	"strconv"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// normalizeCompatOutput keeps at most maxCompatItems items (after dropping
// untitled ones) and fixes kind and line.
func TestNormalizeCompatCapMut(t *testing.T) {
	var bs []output.BreakingChange
	bs = append(bs, output.BreakingChange{Path: "x.go", Title: "  "})
	for i := 0; i < maxCompatItems+5; i++ {
		bs = append(bs, output.BreakingChange{Path: "./a.go", Line: -3, Kind: " API ", Title: "t" + strconv.Itoa(i)})
	}
	out := &output.ReviewOutput{Compatibility: &output.Compatibility{Breaking: bs}}
	NormalizeOutput(out, true)
	got := out.Compatibility.Breaking
	if len(got) != maxCompatItems || got[0].Title != "t0" || got[maxCompatItems-1].Title != "t"+strconv.Itoa(maxCompatItems-1) {
		t.Fatalf("%d items: first %+v", len(got), got[0])
	}
	if got[0].Kind != output.CompatKindAPI || got[0].Line != 0 || got[0].Path != "a.go" {
		t.Errorf("normalized item %+v", got[0])
	}
}
