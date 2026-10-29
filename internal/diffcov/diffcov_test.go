package diffcov

import (
	"strings"
	"testing"
)

const cobertura = `<?xml version="1.0" ?>
<!DOCTYPE coverage SYSTEM "http://cobertura.sourceforge.net/xml/coverage-04.dtd">
<coverage line-rate="0.5" version="7.4">
  <sources><source>/builds/team/api</source></sources>
  <packages><package name="app"><classes>
    <class name="orders.py" filename="app/orders.py" line-rate="0.5">
      <methods/>
      <lines>
        <line number="1" hits="1"/>
        <line number="3" hits="0"/>
        <line number="4" hits="0"/>
        <line number="6" hits="0"/>
        <line number="7" hits="2"/>
      </lines>
    </class>
  </classes></package></packages>
</coverage>`

const clover = `<?xml version="1.0" encoding="UTF-8"?>
<coverage generated="1700000000">
  <project timestamp="1700000000">
    <package name="App">
      <file name="/builds/team/billing/src/Invoice.php">
        <class name="Invoice" namespace="App"><metrics/></class>
        <line num="10" type="method" name="total" count="3"/>
        <line num="11" type="stmt" count="3"/>
        <line num="12" type="stmt" count="0"/>
        <line num="14" type="cond" count="0"/>
      </file>
    </package>
    <file name="/builds/team/billing/src/Util.php">
      <line num="5" type="stmt" count="1"/>
    </file>
  </project>
</coverage>`

const goProfile = `mode: set
github.com/team/svc/internal/orders/service.go:10.2,12.3 2 1
github.com/team/svc/internal/orders/service.go:14.2,16.3 1 0
github.com/team/svc/internal/orders/service.go:15.5,15.20 1 1
`

const lcov = `TN:
SF:src/api/client.ts
DA:3,1
DA:4,0
end_of_record
`

func TestParseFormats(t *testing.T) {
	for name, tc := range map[string]struct {
		data   string
		format Format
		file   string
		line   int
		hits   int
		lines  int
	}{
		"cobertura": {cobertura, FormatCobertura, "/builds/team/api/app/orders.py", 7, 2, 5},
		"clover":    {clover, FormatClover, "/builds/team/billing/src/Invoice.php", 12, 0, 4},
		"go":        {goProfile, FormatGo, "github.com/team/svc/internal/orders/service.go", 15, 1, 6},
		"lcov":      {lcov, FormatLCOV, "src/api/client.ts", 3, 1, 2},
	} {
		t.Run(name, func(t *testing.T) {
			rep, err := Parse([]byte(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			if rep.Format != tc.format {
				t.Errorf("format %s", rep.Format)
			}
			lines, ok := rep.Files[tc.file]
			if !ok {
				t.Fatalf("file %q missing: %v", tc.file, keys(rep.Files))
			}
			if lines[tc.line] != tc.hits || len(lines) != tc.lines {
				t.Errorf("lines %v", lines)
			}
		})
	}
	if _, err := Parse([]byte("hello")); err == nil {
		t.Error("unknown format must fail")
	}
	if _, err := Parse([]byte("<html></html>")); err == nil {
		t.Error("non-coverage xml must fail")
	}
}

func keys(m map[string]map[int]int) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestChangedLines(t *testing.T) {
	if got := ChangedFromHunks("@@ -1,3 +1,5 @@\n a\n+b\n+c\n d\n-x\n e\n@@ -20,2 +22,3 @@\n f\n+g\n\\ No newline at end of file\n"); join(got) != "2,3,23" {
		t.Errorf("hunks %v", got)
	}
	diff := `diff --git a/app/orders.py b/app/orders.py
index 1..2 100644
--- a/app/orders.py
+++ b/app/orders.py
@@ -2,0 +3,2 @@ def f():
+    x = 1
+    y = 2
@@ -5,1 +7,1 @@
-    old
+    new
diff --git a/gone.py b/gone.py
--- a/gone.py
+++ /dev/null
@@ -1 +0,0 @@
-x
diff --git a/new.py b/new.py
new file mode 100644
--- /dev/null
+++ b/new.py
@@ -0,0 +1,2 @@
+a
+b
`
	ch := ChangedFromUnified(diff)
	if join(ch["app/orders.py"]) != "3,4,7" || join(ch["new.py"]) != "1,2" || len(ch) != 2 {
		t.Errorf("unified %v", ch)
	}
}

func join(xs []int) string {
	var s []string
	for _, x := range xs {
		s = append(s, itoa(x))
	}
	return strings.Join(s, ",")
}

func itoa(n int) string { return Range{n, n}.String() }

func TestCompute(t *testing.T) {
	rep, _ := Parse([]byte(cobertura))
	ch := Changed{
		"app/orders.py":       {1, 2, 3, 4, 6, 7, 7}, // 2 is not executable; 7 duplicated
		"app/missing.py":      {1},
		"tests/test_x.py":     {1},
		"docs/readme.md":      {1},
		"app/other/x_test.go": {1},
	}
	res := Compute(rep, ch, nil)
	if res.Total != 5 || res.Covered != 2 || len(res.Files) != 1 {
		t.Fatalf("res %+v", res)
	}
	f := res.Files[0]
	// 3,4 and 6 are separated only by the non-executable line 5 → one range.
	if len(f.Uncovered) != 1 || f.Uncovered[0].String() != "3-6" || f.ReportPath != "/builds/team/api/app/orders.py" {
		t.Errorf("file %+v", f)
	}
	if strings.Join(res.NotInReport, ",") != "app/missing.py" {
		t.Errorf("not in report %v", res.NotInReport)
	}
	if p := res.Percent(); p != 40 {
		t.Errorf("percent %v", p)
	}
	if (Result{}).Percent() != 100 {
		t.Error("no executable changed lines = 100%")
	}

	// Clover: absolute runner paths matched by suffix; include filter.
	rep, _ = Parse([]byte(clover))
	res = Compute(rep, Changed{"src/Invoice.php": {11, 12, 14}, "src/Util.php": {5}}, func(p string) bool { return p != "src/Util.php" })
	if res.Total != 3 || res.Covered != 1 || len(res.Files) != 1 || res.Files[0].Uncovered[0].String() != "12-14" {
		t.Errorf("clover %+v", res)
	}

	// Go: import paths vs repository paths; source-root-relative reports.
	rep, _ = Parse([]byte(goProfile))
	res = Compute(rep, Changed{"internal/orders/service.go": {11, 15, 16}}, nil)
	if res.Total != 3 || res.Covered != 2 {
		t.Errorf("go %+v", res)
	}
	rep = &Report{Files: map[string]map[int]int{"orders.py": {1: 0}, "app/orders.py": {1: 1}}}
	if rp, _ := match(rep, "src/app/orders.py"); rp != "app/orders.py" {
		t.Errorf("longest suffix wins: %s", rp)
	}
}
