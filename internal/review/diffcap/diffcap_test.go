package diffcap

import (
	"strings"
	"testing"
)

func TestSplitHunkLines(t *testing.T) {
	diff := "diff --git a/m.sql b/m.sql\n--- a/m.sql\n+++ b/m.sql\n@@ -1,2 +1,2 @@\n--- old comment\n+-- new comment\n-++ x\n+++ y\n"
	chunks := Split(diff)
	if len(chunks) != 1 || chunks[0].Path != "m.sql" || chunks[0].Changed != 4 {
		t.Fatalf("chunks %+v", chunks)
	}
	// Without a hunk header (binary, mode-only): header lines still name the file.
	chunks = Split("diff --git a/x b/x\n--- a/x\n+++ /dev/null\n")
	if len(chunks) != 1 || chunks[0].Path != "x" || chunks[0].Changed != 0 {
		t.Fatalf("deleted %+v", chunks)
	}
	chunks = Split("preamble\ndiff --git a/a b/a\n+++ b/a\n+1\n")
	if len(chunks) != 2 || chunks[0].Path != "" || chunks[1].Path != "a" || chunks[1].Changed != 1 {
		t.Fatalf("preamble %+v", chunks)
	}
}

func TestCapMut(t *testing.T) {
	mk := func(name string, n int) string {
		s := "diff --git a/" + name + " b/" + name + "\n--- a/" + name + "\n+++ b/" + name + "\n@@ -0,0 +1 @@\n"
		for i := 0; i < n; i++ {
			s += "+line\n"
		}
		return s
	}
	big, mid, small := mk("big.go", 40), mk("mid.go", 10), mk("small.go", 3)
	diff := small + big + mid
	// The biggest file does not fit the byte budget: skipped, the others fit
	// (in their original order).
	res := Cap(diff, 0, len(mid)+len(small))
	if res.Diff != small+mid || res.Reviewed != 2 || res.Total != 3 || res.Truncated ||
		len(res.Omitted) != 1 || res.Omitted[0] != "big.go" {
		t.Fatalf("bytes: %+v", res)
	}
	// Exactly at the limits: nothing is cut.
	if res := Cap(diff, 3, len(diff)); res.Diff != diff || res.Reviewed != 3 || len(res.Omitted) != 0 {
		t.Fatalf("at limits: %+v", res)
	}
	res = Cap(diff, 1, 0)
	if res.Diff != big || res.Reviewed != 1 || strings.Join(res.Omitted, ",") != "small.go,mid.go" {
		t.Fatalf("files: %+v", res)
	}
	// Nothing fits: the most changed file, cut at a line boundary.
	res = Cap(diff, 0, 60)
	if !res.Truncated || res.Reviewed != 1 || len(res.Diff) > 60 || !strings.HasPrefix(big, res.Diff) ||
		!strings.HasSuffix(res.Diff, "\n") || len(res.Omitted) != 2 {
		t.Fatalf("cut: %+v", res)
	}
	if got := cutAtLine("ab\ncd\nef", 7); got != "ab\ncd\n" {
		t.Fatalf("cutAtLine %q", got)
	}
	if got := cutAtLine("abc", 3); got != "abc" {
		t.Fatalf("cutAtLine fits %q", got)
	}
}

func TestSplitHeaders(t *testing.T) {
	// Binary change: no ---/+++ lines, the "diff --git" header names the file.
	chunks := Split("diff --git a/img/a.png b/img/a.png\nBinary files a/img/a.png and b/img/a.png differ\n")
	if len(chunks) != 1 || chunks[0].Path != "img/a.png" {
		t.Fatalf("binary %+v", chunks)
	}
	// Timestamps after a tab and quoted names.
	chunks = Split("diff --git a/x b/x\n--- \"a/old name\"\t2024-01-01\n+++ \"b/new name\"\t2024-01-02\n@@ -1 +1 @@\n-a\n+b\n")
	if len(chunks) != 1 || chunks[0].Path != "new name" || chunks[0].Changed != 2 {
		t.Fatalf("tab/quotes %+v", chunks)
	}
}

// Files with the same number of changed lines keep their order when the
// file limit applies.
func TestCapTies(t *testing.T) {
	mk := func(name string) string {
		return "diff --git a/" + name + " b/" + name + "\n+++ b/" + name + "\n@@ -0,0 +1 @@\n+x\n"
	}
	res := Cap(mk("a.go")+mk("b.go")+mk("c.go"), 2, 0)
	if res.Diff != mk("a.go")+mk("b.go") || strings.Join(res.Omitted, ",") != "c.go" {
		t.Fatalf("ties: %+v", res)
	}
}

func TestCap(t *testing.T) {
	mk := func(name string, n int) string {
		s := "diff --git a/" + name + " b/" + name + "\n--- a/" + name + "\n+++ b/" + name + "\n@@ -1 +1," + "9 @@\n"
		for i := 0; i < n; i++ {
			s += "+line\n"
		}
		return s
	}
	diff := mk("a.go", 2) + mk("b.go", 10) + mk("c.go", 5)
	res := Cap(diff, 2, 0)
	if res.Reviewed != 2 || res.Total != 3 || len(res.Omitted) != 1 || res.Omitted[0] != "a.go" {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(res.Diff, "b/b.go") || !strings.Contains(res.Diff, "b/c.go") || strings.Index(res.Diff, "b.go") > strings.Index(res.Diff, "c.go") {
		t.Fatalf("order/content: %s", res.Diff)
	}
	res = Cap(diff, 0, len(mk("b.go", 10))+5)
	if res.Reviewed != 1 || len(res.Omitted) != 2 {
		t.Fatalf("%+v", res)
	}
	res = Cap(diff, 0, 50)
	if !res.Truncated || res.Reviewed != 1 || len(res.Diff) > 50 {
		t.Fatalf("%+v", res)
	}
	if res := Cap(diff, 10, 1<<20); res.Diff != diff || res.Reviewed != 3 {
		t.Fatalf("no cap: %+v", res)
	}
}
