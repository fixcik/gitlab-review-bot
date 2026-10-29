package repo

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestHunksTouch(t *testing.T) {
	d := []byte("diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -10,2 +10,3 @@\n-x\n-y\n+a\n+b\n+c\n@@ -30,0 +32,1 @@\n+ins\n")
	cases := []struct {
		start, end int
		want       bool
	}{
		{1, 5, false},
		{7, 9, false},   // ends right before the hunk
		{11, 14, true},  // overlaps line 11
		{12, 20, false}, // after the first hunk, before the insertion
		{25, 30, true},  // insertion after line 30 is at the window edge
		{31, 35, true},  // insertion between 30 and 31
		{32, 35, false}, // insertion before the window
		{0, 0, true},    // whole file
	}
	for _, c := range cases {
		if got := hunksTouch(d, c.start, c.end); got != c.want {
			t.Errorf("hunksTouch(%d,%d) = %v, want %v", c.start, c.end, got, c.want)
		}
	}
	if hunksTouch(nil, 0, 0) {
		t.Fatal("empty diff touches nothing")
	}
	if !hunksTouch([]byte("Binary files a/x and b/x differ\n"), 3, 4) {
		t.Fatal("binary change counts")
	}
}

func TestLinesChanged(t *testing.T) {
	r := newRemote(t)
	var lines []string
	for i := 1; i <= 40; i++ {
		lines = append(lines, "line")
	}
	r.write("a.go", strings.Join(lines, "\n")+"\n")
	r.write("b.go", "x\n")
	posted := r.commit("posted")
	lines[29] = "changed" // line 30
	r.write("a.go", strings.Join(lines, "\n")+"\n")
	head := r.commit("fix")
	r.setMRHead(1, head)

	m := newManager(t, filepath.Dir(filepath.Dir(r.dir)))
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", &core.MergeRequest{IID: 1, TargetBranch: "main", DiffRefs: core.DiffRefs{BaseSHA: posted, StartSHA: posted, HeadSHA: head}})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	c := co.(*Checkout)
	if ch, err := c.LinesChanged(ctx, posted, "", "a.go", 27, 33); err != nil || !ch {
		t.Fatalf("window around line 30: %v %v", ch, err)
	}
	if ch, err := c.LinesChanged(ctx, posted, "", "a.go", 1, 7); err != nil || ch {
		t.Fatalf("lines 1-7 unchanged: %v %v", ch, err)
	}
	if ch, _ := c.LinesChanged(ctx, posted, head, "b.go", 0, 0); ch {
		t.Fatal("b.go unchanged")
	}
	if ch, _ := c.LinesChanged(ctx, head, "", "a.go", 27, 33); ch {
		t.Fatal("same commit: nothing changed")
	}
	if ch, err := c.LinesChanged(ctx, posted, head, "a.go", 29, 31); err != nil || !ch {
		t.Fatalf("explicit to: %v %v", ch, err)
	}
	if _, err := c.LinesChanged(ctx, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "", "a.go", 1, 2); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown commit: %v", err)
	}
	if _, err := c.LinesChanged(ctx, posted, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "a.go", 1, 2); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown to commit: %v", err)
	}
	for _, bad := range [][2]string{{"--output=x", ""}, {posted, "--output=x"}} {
		if _, err := c.LinesChanged(ctx, bad[0], bad[1], "a.go", 1, 2); err == nil {
			t.Fatalf("invalid revision %v must fail", bad)
		}
	}
}
