package diffmap

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// visible lists the visible new lines 1..n as "line:old" ("line:+" for an
// added line).
func visible(m *Map, path string, n int) []string {
	var out []string
	for l := 1; l <= n; l++ {
		pos, ok := m.Anchor(refs, path, l)
		if !ok {
			continue
		}
		if pos.OldLine == 0 {
			out = append(out, strconv.Itoa(l)+":+")
		} else {
			out = append(out, strconv.Itoa(l)+":"+strconv.Itoa(pos.OldLine))
		}
	}
	return out
}

// TestHunkLineNumbers: removed lines advance only the old side, added lines
// only the new side, context lines both — across hunks whose new start
// differs from the old one.
func TestHunkLineNumbers(t *testing.T) {
	diff := "diff --git a/f b/f\n--- a/f\n+++ b/f\n" +
		"@@ -3,5 +3,4 @@\n c3\n-r4\n-r5\n+a4\n c6\n c7\n" +
		"@@ -40,2 +39,3 @@\n+a39\n c40\n+a41\n-r41\n"
	m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: diff}})
	want := []string{"3:3", "4:+", "5:6", "6:7", "39:+", "40:40", "41:+"}
	if got := visible(m, "f", 60); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// TestHunkEndsOnExhaustedCounts: once a side's count is used up, a line of
// that side (or a context line) ends the hunk instead of being recorded.
func TestHunkEndsOnExhaustedCounts(t *testing.T) {
	cases := []struct {
		name, diff string
		want       []string
	}{
		{"removed past old count", "@@ -1,1 +1,2 @@\n-a\n-b\n+c\n+d\n", nil},
		{"context past new count", "@@ -1,2 +1,1 @@\n+x\n y\n", []string{"1:+"}},
		{"context past old count", "@@ -1,1 +1,3 @@\n-a\n z\n", nil},
		{"lines after exhausted hunk", "@@ -1,1 +1,1 @@\n a\n b\n+c\n", []string{"1:1"}},
		{"empty hunk", "@@ -1,0 +0,0 @@\n+x\n", nil},
		{"next file header ends hunk", "@@ -1,3 +1,3 @@\n a\ndiff --git a/g b/g\n b\n", []string{"1:1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: c.diff}})
			if got := visible(m, "f", 10); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

// TestBuildPaths: a file is keyed by its new path (old path when there is
// none); a missing old path is the new one; entries without any path are
// dropped.
func TestBuildPaths(t *testing.T) {
	m := Build([]core.FileDiff{
		{NewPath: "only_new", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
		{OldPath: "only_old", Diff: "@@ -1 +1 @@\n x\n"},
		{Diff: "@@ -1 +1 @@\n+z\n"},
	})
	if got := m.Files(); !reflect.DeepEqual(got, []string{"only_new", "only_old"}) {
		t.Fatalf("files %v", got)
	}
	pos, ok := m.Anchor(refs, "only_new", 1)
	if !ok || pos.OldPath != "only_new" || pos.NewPath != "only_new" {
		t.Errorf("only_new: %+v %v", pos, ok)
	}
	pos, ok = m.Anchor(refs, "only_old", 1)
	if !ok || pos.OldPath != "only_old" || pos.NewPath != "only_old" || pos.OldLine != 1 {
		t.Errorf("only_old: %+v %v", pos, ok)
	}
}

// TestBuildRepeatedPath: several entries for one path merge; a deletion
// wipes the lines and later entries for that path are ignored.
func TestBuildRepeatedPath(t *testing.T) {
	m := Build([]core.FileDiff{
		{OldPath: "f", NewPath: "f", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
		{OldPath: "f", NewPath: "f", Diff: "@@ -10 +10 @@\n-a\n+b\n"},
	})
	if got := visible(m, "f", 20); !reflect.DeepEqual(got, []string{"1:+", "10:+"}) {
		t.Errorf("merged: %v", got)
	}
	m = Build([]core.FileDiff{
		{OldPath: "g", NewPath: "g", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
		{OldPath: "g", NewPath: "g", DeletedFile: true},
		{OldPath: "g", NewPath: "g", Diff: "@@ -5 +5 @@\n-a\n+b\n"},
	})
	if got := visible(m, "g", 20); got != nil || !m.HasFile("g") || len(m.Files()) != 1 {
		t.Errorf("deleted: %v files %v", got, m.Files())
	}
	if _, ok := m.Snap("g", 5, 10); ok {
		t.Error("deleted file snaps")
	}
}

// TestFilesReturnsCopy: callers may modify the returned slice.
func TestFilesReturnsCopy(t *testing.T) {
	m := Build([]core.FileDiff{{OldPath: "b", NewPath: "b"}, {OldPath: "a", NewPath: "a"}})
	fs := m.Files()
	fs[0] = "x"
	if got := m.Files(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("files %v", got)
	}
}

// TestSnapTies pins Snap at equal distance: an added line beats a context
// line on either side, two lines of the same kind pick the lower one.
func TestSnapTies(t *testing.T) {
	cases := []struct {
		diff string
		want int
	}{
		{"@@ -4,1 +4,1 @@\n a\n@@ -6,1 +6,1 @@\n b\n", 4},     // ctx 4, ctx 6
		{"@@ -4,0 +4,1 @@\n+a\n@@ -6,0 +6,1 @@\n+b\n", 4},     // add 4, add 6
		{"@@ -4,0 +4,1 @@\n+a\n@@ -5,1 +6,1 @@\n b\n", 4},     // add 4, ctx 6
		{"@@ -4,1 +4,1 @@\n a\n@@ -6,0 +6,1 @@\n+b\n", 6},     // ctx 4, add 6
		{"@@ -4,1 +4,2 @@\n a\n+x\n@@ -6,1 +7,1 @@\n b\n", 5}, // exact beats both
	}
	for _, c := range cases {
		m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: c.diff}})
		if got, ok := m.Snap("f", 5, 1); !ok || got != c.want {
			t.Errorf("%q: got %d %v, want %d", c.diff, got, ok, c.want)
		}
	}
}

// TestOmittedCountIsOne: "@@ -5 +5 @@" covers exactly one line per side;
// a line past it is not part of the hunk.
func TestOmittedCountIsOne(t *testing.T) {
	m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: "@@ -5 +5 @@\n a\n b\n"}})
	if got := visible(m, "f", 10); !reflect.DeepEqual(got, []string{"5:5"}) {
		t.Errorf("got %v", got)
	}
}

// TestSnapNegativeDistance: a negative distance means exact lines only.
func TestSnapNegativeDistance(t *testing.T) {
	m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: "@@ -10,1 +10,1 @@\n a\n"}})
	if got, ok := m.Snap("f", 11, -1); ok {
		t.Errorf("snapped to %d", got)
	}
	if got, ok := m.Snap("f", 10, -5); !ok || got != 10 {
		t.Errorf("exact: %d %v", got, ok)
	}
}
