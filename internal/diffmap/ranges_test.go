package diffmap

import (
	"reflect"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestRanges(t *testing.T) {
	m := Build([]core.FileDiff{
		{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -1,3 +1,5 @@\n a\n+b\n+c\n d\n e\n@@ -20,2 +22,3 @@\n x\n+y\n z\n"},
		{OldPath: "gone.go", NewPath: "gone.go", DeletedFile: true, Diff: "@@ -1 +0,0 @@\n-a\n"},
	})
	added, visible := m.Ranges("a.go")
	if want := [][2]int{{2, 3}, {23, 23}}; !reflect.DeepEqual(added, want) {
		t.Errorf("added %v, want %v", added, want)
	}
	if want := [][2]int{{1, 5}, {22, 24}}; !reflect.DeepEqual(visible, want) {
		t.Errorf("visible %v, want %v", visible, want)
	}
	if a, v := m.Ranges("gone.go"); a != nil || v != nil {
		t.Errorf("deleted file: %v %v", a, v)
	}
	if a, v := m.Ranges("nope.go"); a != nil || v != nil {
		t.Errorf("unknown file: %v %v", a, v)
	}
	var nilMap *Map
	if a, v := nilMap.Ranges("a.go"); a != nil || v != nil {
		t.Error("nil map")
	}
}
