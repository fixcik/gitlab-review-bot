package diffcov

import "testing"

// Several report files end with the repository path: the one with the
// fewest extra leading directories is the file (a root main.go next to
// cmd/x/main.go, Go import paths, absolute runner paths).
func TestMatchTiePrefersShortestReportPath(t *testing.T) {
	rep := &Report{Files: map[string]map[int]int{
		"/builds/g/p/main.go":          {1: 1},
		"/builds/g/p/cmd/tool/main.go": {1: 0},
		"/builds/g/p/app/main.go":      {1: 0},
	}}
	if rp, ok := match(rep, "main.go"); !ok || rp != "/builds/g/p/main.go" {
		t.Errorf("main.go -> %q", rp)
	}
	if rp, _ := match(rep, "cmd/tool/main.go"); rp != "/builds/g/p/cmd/tool/main.go" {
		t.Errorf("cmd/tool/main.go -> %q", rp)
	}
	// Same length: deterministic (lexicographic).
	rep = &Report{Files: map[string]map[int]int{"/b/y/x.go": {1: 1}, "/b/a/x.go": {1: 1}}}
	if rp, _ := match(rep, "x.go"); rp != "/b/a/x.go" {
		t.Errorf("tie -> %q", rp)
	}
	res := Compute(&Report{Files: map[string]map[int]int{
		"github.com/team/svc/main.go":       {3: 1},
		"github.com/team/svc/cmd/x/main.go": {3: 0},
	}}, Changed{"main.go": {3}}, nil)
	if res.Covered != 1 || res.Total != 1 {
		t.Errorf("root main.go measured against another file: %+v", res)
	}
}

// istanbul/Jest clover: name is the base name, path the file's full path.
func TestCloverNameAndFullPath(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<coverage generated="1"><project timestamp="1" name="All files">
  <file name="calc.js" path="/builds/team/web/src/calc.js"><line num="3" count="1" type="stmt"/></file>
  <file name="util.js" path="C:\builds\web\src\lib\"><line num="4" count="0" type="stmt"/></file>
</project></coverage>`
	rep, err := Parse([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rep.Files["/builds/team/web/src/calc.js"]; !ok {
		t.Errorf("full path: %v", keys(rep.Files))
	}
	if _, ok := rep.Files["C:/builds/web/src/lib/util.js"]; !ok {
		t.Errorf("directory path: %v", keys(rep.Files))
	}
	res := Compute(rep, Changed{"src/calc.js": {3}, "src/lib/util.js": {4}}, nil)
	if res.Total != 2 || res.Covered != 1 || len(res.NotInReport) != 0 {
		t.Errorf("res %+v", res)
	}
}

func TestParseReportCorners(t *testing.T) {
	// Several <source>s: filenames stay relative (no guessing which one).
	cob := `<coverage><sources><source>/a</source><source>/b</source></sources><packages><package><classes>
<class filename="x.py"><lines><line number="1" hits="1"/></lines></class></classes></package></packages></coverage>`
	rep, err := Parse([]byte(cob))
	if err != nil || rep.Files["x.py"] == nil {
		t.Errorf("two sources: %v %v", keys(rep.Files), err)
	}
	// Lenient XML: an unescaped ampersand does not fail the report.
	rep, err = Parse([]byte(`<coverage><packages><package><classes><class filename="a&b.py"><lines><line number="2" hits="0"/></lines></class></classes></package></packages></coverage>`))
	if err != nil || len(rep.Files) != 1 {
		t.Errorf("lenient xml: %v %v", rep, err)
	}
	// Concatenated go profiles: blank lines and repeated mode lines.
	rep, err = Parse([]byte("mode: set\na.go:1.1,2.2 1 1\n\nmode: set\nb.go:3.1,3.9 1 0\n"))
	if err != nil || len(rep.Files["a.go"]) != 2 || len(rep.Files["b.go"]) != 1 {
		t.Errorf("go concat: %v %v", rep, err)
	}
}

func TestComputeCorners(t *testing.T) {
	rep := &Report{Files: map[string]map[int]int{"a.py": {1: 0, 2: 1, 3: 0, 5: 0}, "b.py": {9: 1}}}
	res := Compute(rep, Changed{"a.py": {1, 3, 5}, "b.py": {1, 2}, "src/c.ts": {1}}, nil)
	if len(res.Files) != 1 || res.Files[0].Path != "a.py" {
		t.Fatalf("a file without executable changed lines is not listed: %+v", res.Files)
	}
	// Covered line 2 splits 1 from 3; 4 is not executable and does not.
	if got := RangesText(res.Files[0].Uncovered, 0); got != "1, 3-5" {
		t.Errorf("uncovered = %s", got)
	}
	if len(res.NotInReport) != 1 || res.NotInReport[0] != "src/c.ts" {
		t.Errorf("not in report %v", res.NotInReport)
	}
	ch := ChangedFromUnified("diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,1 +1,2 @@\n a\n+b\n")
	if join(ch["x.go"]) != "2" {
		t.Errorf("one added line: %v", ch)
	}
}
