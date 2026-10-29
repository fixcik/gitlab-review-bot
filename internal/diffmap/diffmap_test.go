package diffmap

import (
	"reflect"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

var refs = core.DiffRefs{BaseSHA: "base", StartSHA: "start", HeadSHA: "head"}

const multiHunk = `@@ -1,4 +1,5 @@
 package main

+import "fmt"
+
 func a() {}
-func b() {}
@@ -20,3 +21,4 @@ func c() {
 	x := 1
-	y := 2
+	y := 3
+	z := 4
 	return
`

func TestAnchorMultiHunk(t *testing.T) {
	m := Build([]core.FileDiff{{OldPath: "main.go", NewPath: "main.go", Diff: multiHunk}})

	tests := []struct {
		name    string
		line    int
		ok      bool
		oldLine int
	}{
		{"context first line", 1, true, 1},
		{"context blank line", 2, true, 2},
		{"added import", 3, true, 0},
		{"added blank", 4, true, 0},
		{"context after adds", 5, true, 3},
		{"gap between hunks", 6, false, 0},
		{"gap line 20", 20, false, 0},
		{"second hunk context", 21, true, 20},
		{"second hunk added y", 22, true, 0},
		{"second hunk added z", 23, true, 0},
		{"second hunk trailing context", 24, true, 22},
		{"past end", 25, false, 0},
		{"zero", 0, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pos, ok := m.Anchor(refs, "main.go", tt.line)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				if pos != nil {
					t.Fatalf("pos = %+v, want nil", pos)
				}
				return
			}
			want := &core.Position{
				BaseSHA: "base", StartSHA: "start", HeadSHA: "head",
				OldPath: "main.go", NewPath: "main.go",
				OldLine: tt.oldLine, NewLine: tt.line,
			}
			if !reflect.DeepEqual(pos, want) {
				t.Fatalf("pos = %+v, want %+v", pos, want)
			}
		})
	}
}

func TestRename(t *testing.T) {
	m := Build([]core.FileDiff{{
		OldPath: "old/name.go", NewPath: "new/name.go", RenamedFile: true,
		Diff: "@@ -10,2 +10,2 @@\n keep\n-old\n+new\n",
	}})
	if m.HasFile("old/name.go") {
		t.Fatal("lookup must be by new path")
	}
	if !m.HasFile("new/name.go") {
		t.Fatal("new path missing")
	}
	pos, ok := m.Anchor(refs, "new/name.go", 10)
	if !ok || pos.OldPath != "old/name.go" || pos.NewPath != "new/name.go" || pos.OldLine != 10 || pos.NewLine != 10 {
		t.Fatalf("context anchor = %+v %v", pos, ok)
	}
	pos, ok = m.Anchor(refs, "new/name.go", 11)
	if !ok || pos.OldLine != 0 || pos.NewLine != 11 || pos.OldPath != "old/name.go" {
		t.Fatalf("added anchor = %+v %v", pos, ok)
	}
}

func TestRenameWithoutChanges(t *testing.T) {
	m := Build([]core.FileDiff{{OldPath: "a.go", NewPath: "b.go", RenamedFile: true}})
	if !m.HasFile("b.go") {
		t.Fatal("renamed file missing")
	}
	if _, ok := m.Anchor(refs, "b.go", 1); ok {
		t.Fatal("pure rename has no visible lines")
	}
	if _, ok := m.Snap("b.go", 1, 10); ok {
		t.Fatal("pure rename snap must fail")
	}
}

func TestNewFile(t *testing.T) {
	m := Build([]core.FileDiff{{
		OldPath: "n.txt", NewPath: "n.txt", NewFile: true,
		Diff: "@@ -0,0 +1,3 @@\n+one\n+two\n+three\n",
	}})
	for l := 1; l <= 3; l++ {
		pos, ok := m.Anchor(refs, "n.txt", l)
		if !ok || pos.OldLine != 0 || pos.NewLine != l {
			t.Fatalf("line %d: %+v %v", l, pos, ok)
		}
	}
	if _, ok := m.Anchor(refs, "n.txt", 4); ok {
		t.Fatal("line 4 must not exist")
	}
}

func TestDeletedFile(t *testing.T) {
	m := Build([]core.FileDiff{{
		OldPath: "gone.txt", NewPath: "gone.txt", DeletedFile: true,
		Diff: "@@ -1,2 +0,0 @@\n-one\n-two\n",
	}})
	if !m.HasFile("gone.txt") {
		t.Fatal("deleted file should be listed")
	}
	for l := 0; l <= 3; l++ {
		if _, ok := m.Anchor(refs, "gone.txt", l); ok {
			t.Fatalf("deleted file line %d must not anchor", l)
		}
	}
	if _, ok := m.Snap("gone.txt", 1, 100); ok {
		t.Fatal("deleted file snap must fail")
	}
}

func TestNoNewlineMarker(t *testing.T) {
	diff := "@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n\\ No newline at end of file\n"
	m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: diff}})
	if pos, ok := m.Anchor(refs, "f", 1); !ok || pos.OldLine != 1 {
		t.Fatalf("line 1: %+v %v", pos, ok)
	}
	if pos, ok := m.Anchor(refs, "f", 2); !ok || pos.OldLine != 0 {
		t.Fatalf("line 2: %+v %v", pos, ok)
	}
	if _, ok := m.Anchor(refs, "f", 3); ok {
		t.Fatal("marker must not create a line")
	}
}

func TestOmittedCounts(t *testing.T) {
	tests := []struct {
		name    string
		diff    string
		line    int
		oldLine int
		added   bool
	}{
		{"both omitted", "@@ -5 +5 @@\n-x\n+y\n", 5, 0, true},
		{"new omitted", "@@ -3,2 +3 @@\n-x\n-y\n+z\n", 3, 0, true},
		{"old omitted context", "@@ -7 +7,2 @@\n ctx\n+add\n", 7, 7, false},
		{"old omitted added", "@@ -7 +7,2 @@\n ctx\n+add\n", 8, 0, true},
		{"single new file line", "@@ -0,0 +1 @@\n+only\n", 1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: tt.diff}})
			pos, ok := m.Anchor(refs, "f", tt.line)
			if !ok {
				t.Fatalf("line %d not visible", tt.line)
			}
			if pos.OldLine != tt.oldLine {
				t.Fatalf("OldLine = %d, want %d", pos.OldLine, tt.oldLine)
			}
		})
	}
}

func TestHunkCountsStopParsing(t *testing.T) {
	// Garbage after the hunk is exhausted must not create lines.
	diff := "@@ -1,1 +1,1 @@\n-a\n+b\n+not part of hunk\n"
	m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: diff}})
	if _, ok := m.Anchor(refs, "f", 2); ok {
		t.Fatal("line beyond hunk count must not be visible")
	}
}

func TestStrippedContextLine(t *testing.T) {
	// Some tools strip the single space of empty context lines.
	diff := "@@ -1,3 +1,4 @@\n a\n\n+new\n c\n"
	m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: diff}})
	if pos, ok := m.Anchor(refs, "f", 2); !ok || pos.OldLine != 2 {
		t.Fatalf("empty context: %+v %v", pos, ok)
	}
	if pos, ok := m.Anchor(refs, "f", 4); !ok || pos.OldLine != 3 {
		t.Fatalf("line 4: %+v %v", pos, ok)
	}
}

func TestSnap(t *testing.T) {
	// new lines: 10 ctx, 11 added, 12 ctx, 20 ctx, 22 added, 30 ctx
	diff := "@@ -10,2 +10,3 @@\n a\n+b\n c\n" +
		"@@ -19,1 +20,1 @@\n d\n" +
		"@@ -21,1 +22,1 @@\n-x\n+e\n" +
		"@@ -29,1 +30,1 @@\n f\n"
	m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: diff}})

	tests := []struct {
		name    string
		line    int
		maxDist int
		want    int
		ok      bool
	}{
		{"exact added", 11, 0, 11, true},
		{"exact context", 10, 3, 10, true},
		{"prefer added over context at equal distance", 21, 3, 22, true},
		{"within range further away", 25, 5, 22, true},
		{"added beats context at distance 4", 26, 5, 22, true},
		{"nearest wins", 13, 5, 12, true},
		{"out of range", 16, 3, 0, false},
		{"context tie picks lower", 16, 4, 12, true},
		{"zero dist miss", 13, 0, 0, false},
		{"negative dist treated as zero", 11, -1, 11, true},
		{"before first", 7, 3, 10, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := m.Snap("f", tt.line, tt.maxDist)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("Snap(%d,%d) = %d,%v want %d,%v", tt.line, tt.maxDist, got, ok, tt.want, tt.ok)
			}
		})
	}
	if _, ok := m.Snap("missing", 1, 10); ok {
		t.Fatal("missing file must not snap")
	}
}

func TestSnapAddedBeatsLowerContext(t *testing.T) {
	// line 4 ctx, line 6 added; snapping 5 must pick 6.
	diff := "@@ -4,1 +4,1 @@\n a\n@@ -6,0 +6,1 @@\n+b\n"
	m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: diff}})
	if got, ok := m.Snap("f", 5, 1); !ok || got != 6 {
		t.Fatalf("got %d %v, want 6", got, ok)
	}
}

func TestFilesAndHasFile(t *testing.T) {
	m := Build([]core.FileDiff{
		{OldPath: "z.go", NewPath: "z.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
		{OldPath: "a_old.go", NewPath: "a.go", RenamedFile: true},
		{OldPath: "d.go", NewPath: "d.go", DeletedFile: true, Diff: "@@ -1 +0,0 @@\n-a\n"},
	})
	want := []string{"a.go", "d.go", "z.go"}
	if got := m.Files(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Files = %v, want %v", got, want)
	}
	if m.HasFile("nope") {
		t.Fatal("unexpected file")
	}
}

func TestNilAndEmpty(t *testing.T) {
	var m *Map
	if m.HasFile("x") || m.Files() != nil {
		t.Fatal("nil map must be empty")
	}
	if _, ok := m.Anchor(refs, "x", 1); ok {
		t.Fatal("nil map anchor")
	}
	if _, ok := m.Snap("x", 1, 3); ok {
		t.Fatal("nil map snap")
	}
	e := Build(nil)
	if len(e.Files()) != 0 {
		t.Fatal("empty build")
	}
}

func TestCRLF(t *testing.T) {
	m := Build([]core.FileDiff{{OldPath: "f", NewPath: "f", Diff: "@@ -1,2 +1,2 @@\r\n a\r\n-b\r\n+c\r\n"}})
	if pos, ok := m.Anchor(refs, "f", 2); !ok || pos.OldLine != 0 {
		t.Fatalf("crlf: %+v %v", pos, ok)
	}
}
