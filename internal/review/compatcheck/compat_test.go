package compatcheck

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func TestCompatMarkerAndChatStrip(t *testing.T) {
	it := Item{ID: "0123456789ab", Source: compat.SourceGraphQL, Path: "s.graphql", Title: "Field `email` was removed <b>"}
	b := noteBody(it, true, 0)
	id, src, ok := marker.ParseCompat(b)
	if !ok || id != "0123456789ab" || src != compat.SourceGraphQL {
		t.Fatalf("marker: %q %q %v", id, src, ok)
	}
	if strings.Contains(b, "<b>") {
		t.Errorf("tool text must be escaped:\n%s", b)
	}
	if _, ok := marker.ParseFinding(b); ok {
		t.Error("compat note must not look like a finding")
	}
	if strings.Contains(marker.StripNoteMarkers(b), "reviewbot:compat") {
		t.Error("chat must strip the compat marker")
	}
	if mrthread.ParseCompatNote(b) == "" {
		t.Error("title not parsed")
	}
}

func TestNormalizeCompatOutput(t *testing.T) {
	out := &output.ReviewOutput{Compatibility: &output.Compatibility{
		Breaking: []output.BreakingChange{{Title: " x ", Kind: "API", Path: "./a.go", Line: -1}, {Title: ""}, {Title: "y", Kind: "weird"}},
		Resolved: []output.ResolvedFinding{{ID: " ABC "}, {ID: ""}},
	}}
	NormalizeOutput(out, true)
	c := out.Compatibility
	if len(c.Breaking) != 2 || c.Breaking[0].Kind != "api" || c.Breaking[0].Path != "a.go" || c.Breaking[0].Line != 0 || c.Breaking[1].Kind != "other" {
		t.Errorf("%+v", c.Breaking)
	}
	if len(c.Resolved) != 1 || c.Resolved[0].ID != "abc" {
		t.Errorf("%+v", c.Resolved)
	}
	NormalizeOutput(out, false)
	if out.Compatibility != nil {
		t.Error("not asked: must be dropped")
	}
	none := &output.ReviewOutput{}
	NormalizeOutput(none, true)
	if none.Compatibility == nil {
		t.Error("asked but missing: empty section")
	}
}
