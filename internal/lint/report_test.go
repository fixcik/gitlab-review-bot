package lint

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// codeQualitySample follows what GitLab's Code Quality widget reads
// (docs.gitlab.com/ci/testing/code_quality: description, check_name,
// fingerprint, severity, location.path, location.lines.begin), as written
// by `phpstan --error-format=gitlab` and `ruff --output-format=gitlab`
// (positions.begin.line).
const codeQualitySample = `[
 {"description":"Variable $undefined might not be defined.","check_name":"variable.undefined","fingerprint":"a1","severity":"major",
  "location":{"path":"src/Report.php","lines":{"begin":3}}},
 {"description":"F401 ` + "`os`" + ` imported but unused","check_name":"F401","fingerprint":"b2","severity":"minor",
  "location":{"path":"app/util.py","positions":{"begin":{"line":1,"column":8},"end":{"line":2,"column":10}}}},
 {"description":"blocker","check_name":"b","fingerprint":"c3","severity":"blocker","location":{"path":"a.go","lines":{"begin":5,"end":7}}},
 {"description":"critical","check_name":"c","fingerprint":"c4","severity":"critical","location":{"path":"a.go","lines":{"begin":5}}},
 {"description":"info","check_name":"i","fingerprint":"c5","severity":"info","location":{"path":"a.go","lines":{"begin":5}}},
 {"description":"no severity","check_name":"","fingerprint":"c6","location":{"path":"a.go","lines":{"begin":6}}},
 {"description":"weird severity","check_name":"w","severity":"HUGE","location":{"path":"a.go","lines":{"begin":6}}},
 {"description":"no line","check_name":"n","severity":"major","location":{"path":"a.go"}},
 {"description":"offset only","check_name":"o","severity":"major","location":{"path":"a.go","positions":{"begin":{"offset":10}}}},
 {"description":"no path","check_name":"p","severity":"major","location":{"lines":{"begin":1}}},
 {"type":"measurement","description":"not an issue","check_name":"m","location":{"path":"a.go","lines":{"begin":1}}}
]`

func TestParseCodeClimate(t *testing.T) {
	fs, err := ParseReport([]byte(codeQualitySample), FormatAuto, "ci:x")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		got = append(got, fmt.Sprintf("%s %s:%d-%d %s", f.Rule, f.Path, f.Line, f.EndLine, f.Severity))
		if f.Tool != "ci:x" {
			t.Errorf("tool %q", f.Tool)
		}
	}
	want := []string{
		"variable.undefined src/Report.php:3-0 major",
		"F401 app/util.py:1-2 minor",
		"b a.go:5-7 critical",
		"c a.go:5-0 critical",
		"i a.go:5-0 nit",
		"x a.go:6-0 minor", // no check_name: the tool's name; no severity: minor
		"w a.go:6-0 minor",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if fs[0].Message != "Variable $undefined might not be defined." {
		t.Errorf("message %q", fs[0].Message)
	}
	for in, want := range map[string]core.Severity{"Blocker": core.SeverityCritical, " major ": core.SeverityMajor, "minor": core.SeverityMinor, "INFO": core.SeverityNit, "": core.SeverityMinor} {
		if got := CodeClimateSeverity(in); got != want {
			t.Errorf("CodeClimateSeverity(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestParseCodeClimateStreamAndErrors(t *testing.T) {
	// The CodeClimate engine format: issue objects separated by NUL.
	stream := `{"type":"issue","check_name":"r1","description":"d","severity":"major","location":{"path":"a.go","lines":{"begin":1}}}` + "\x00" +
		`{"type":"issue","check_name":"r2","description":"d","location":{"path":"a.go","lines":{"begin":2}}}` + "\x00"
	fs, err := ParseReport([]byte(stream), "", "ci:x")
	if err != nil || len(fs) != 2 || fs[1].Rule != "r2" {
		t.Fatalf("stream: %+v %v", fs, err)
	}
	if fs, err := ParseReport([]byte("[]"), FormatCodeClimate, "ci:x"); err != nil || len(fs) != 0 {
		t.Errorf("empty array: %v %v", fs, err)
	}
	for name, in := range map[string]string{
		"empty":      "  \n",
		"broken":     `[{"description":`,
		"not json":   "phpstan: Allowed memory size exhausted",
		"mistyped":   `[{"location":{"path":"a.go","lines":{"begin":"3"}}}]`,
		"bom only":   "\xef\xbb\xbf",
		"bad format": "[]",
	} {
		format := FormatAuto
		if name == "bad format" {
			format = "junit"
		}
		if fs, err := ParseReport([]byte(in), format, "ci:x"); err == nil {
			t.Errorf("%s: must fail, got %+v", name, fs)
		}
	}
}

func TestParseReportDetectionAndGzip(t *testing.T) {
	sarif := sarifWith(1, "a.go")
	checkstyle := `<?xml version="1.0"?><checkstyle><file name="a.go"><error line="1" severity="warning" message="m" source="s"/></file></checkstyle>`
	for name, in := range map[string]string{"sarif": sarif, "checkstyle": checkstyle, "codeclimate": `[{"check_name":"r","description":"m","location":{"path":"a.go","lines":{"begin":1}}}]`} {
		fs, err := ParseReport([]byte("\xef\xbb\xbf"+in+"\n"), "", "ci:x")
		if err != nil || len(fs) != 1 || fs[0].Path != "a.go" || fs[0].Line != 1 {
			t.Errorf("%s: %+v %v", name, fs, err)
		}
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write([]byte(in))
		_ = zw.Close()
		if fs, err := ParseReport(gz.Bytes(), FormatAuto, "ci:x"); err != nil || len(fs) != 1 {
			t.Errorf("%s gzip: %+v %v", name, fs, err)
		}
	}
	// An explicit format wins over detection.
	if _, err := ParseReport([]byte(sarif), FormatCheckstyle, "ci:x"); err == nil {
		t.Error("SARIF read as Checkstyle must fail")
	}
}

func TestParseReportSizeCap(t *testing.T) {
	big := make([]byte, MaxReportBytes+1)
	copy(big, "[]")
	if _, err := ParseReport(big, FormatAuto, "ci:x"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("over the cap: %v", err)
	}
	// A gzip bomb is cut at the cap as well.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("["))
	_, _ = zw.Write(bytes.Repeat([]byte(" "), MaxReportBytes+10))
	_, _ = zw.Write([]byte("]"))
	_ = zw.Close()
	if _, err := ParseReport(gz.Bytes(), FormatAuto, "ci:x"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("unpacked over the cap: %v", err)
	}
}

func TestResolveReportPath(t *testing.T) {
	ch := ParseChangedLines(goDiff) // main.go, app/util.py
	for in, want := range map[string]string{
		"app/util.py":                         "app/util.py",
		"./app/util.py":                       "app/util.py",
		"file:///builds/g/p/app/util.py":      "app/util.py",
		"/builds/group/project/main.go":       "main.go",
		`C:\builds\group\project\app\util.py`: "app/util.py",
		"/elsewhere/other.go":                 "elsewhere/other.go",
		"src/../main.go":                      "main.go",
		"":                                    "",
	} {
		if got := ResolveReportPath(in, ch); got != want {
			t.Errorf("ResolveReportPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChangedFromFileDiffs(t *testing.T) {
	ch := ChangedFromFileDiffs([]core.FileDiff{
		{NewPath: "a.go", Diff: "@@ -1,2 +1,3 @@\n a\n+b\n c\n"},
		{NewPath: "gone.go", DeletedFile: true, Diff: "@@ -1 +0,0 @@\n-x\n"},
		{NewPath: "vendor/x.go", Diff: "@@ -0,0 +1 @@\n+x\n"},
		{NewPath: "dir/new file.py", NewFile: true, Diff: "@@ -0,0 +1,2 @@\n+x\n+y"},
	}, func(p string) bool { return strings.HasPrefix(p, "vendor/") })
	if !ch.Line("a.go", 2) || ch.Line("a.go", 1) || ch.Has("gone.go") || ch.Has("vendor/x.go") || !ch.Line("dir/new file.py", 2) {
		t.Errorf("changed: %v", ch.Files())
	}
}

func TestCheckReport(t *testing.T) {
	ch := ParseChangedLines(goDiff) // main.go: 3, 4, 6; app/util.py: 1-3
	cq := `[
	 {"check_name":"r1","description":"on an added line","severity":"major","location":{"path":"/builds/g/p/main.go","lines":{"begin":3}}},
	 {"check_name":"r2","description":"old code","severity":"major","location":{"path":"main.go","lines":{"begin":1}}},
	 {"check_name":"r3","description":"style","severity":"info","location":{"path":"app/util.py","lines":{"begin":2}}}]`
	fs, st := CheckReport(CIReport{Tool: "ci:phpstan", Data: [][]byte{[]byte(cq), []byte(cq)}}, ch,
		map[string]core.Severity{"ci:phpstan:r3": core.SeverityMinor})
	if st.State != StateRan || st.Total != 3 || st.Found != 2 || st.Tool != "ci:phpstan" {
		t.Fatalf("status %+v", st)
	}
	if len(fs) != 2 || fs[0].Path != "main.go" || fs[0].Rule != "r1" || !fs[0].InDiff || fs[0].MayBlock || fs[1].Severity != core.SeverityMinor {
		t.Fatalf("findings %+v (parallel jobs reporting the same result are one finding)", fs)
	}

	// Entry severity and blocking.
	fs, _ = CheckReport(CIReport{Tool: "ci:arch", Severity: core.SeverityCritical, Blocking: true, Data: [][]byte{[]byte(cq)}}, ch, nil)
	if len(fs) != 2 || fs[0].Severity != core.SeverityCritical || !fs[0].MayBlock {
		t.Errorf("entry severity/blocking: %+v", fs)
	}

	// One unreadable report among two: partial (old threads stay open).
	_, st = CheckReport(CIReport{Tool: "ci:x", Data: [][]byte{[]byte(cq), []byte("garbage")}}, ch, nil)
	if st.State != StatePartial || !strings.Contains(st.Reason, "часть отчётов") {
		t.Errorf("partial: %+v", st)
	}
	// Nothing readable: failed.
	_, st = CheckReport(CIReport{Tool: "ci:x", Data: [][]byte{[]byte("garbage")}}, ch, nil)
	if st.State != StateFailed || !strings.Contains(st.Reason, "не удалось разобрать отчёт") {
		t.Errorf("failed: %+v", st)
	}
	_, st = CheckReport(CIReport{Tool: "ci:x"}, ch, nil)
	if st.State != StateFailed {
		t.Errorf("no data: %+v", st)
	}
	// Explicit format.
	if _, st = CheckReport(CIReport{Tool: "ci:x", Format: FormatSARIF, Data: [][]byte{[]byte(cq)}}, ch, nil); st.State != StateFailed {
		t.Errorf("wrong explicit format: %+v", st)
	}
}

func TestCheckReportCapIsPartial(t *testing.T) {
	ch := ParseChangedLines(bigDiff("big.go", maxFindingsPerTool+1))
	_, st := CheckReport(CIReport{Tool: "ci:many", Data: [][]byte{[]byte(sarifWith(maxFindingsPerTool+1, "big.go"))}}, ch, nil)
	if st.State != StatePartial || st.Found != maxFindingsPerTool || st.Total != maxFindingsPerTool+1 {
		t.Errorf("over the cap: %+v", st)
	}
	_, st = CheckReport(CIReport{Tool: "ci:few", Data: [][]byte{[]byte(sarifWith(maxFindingsPerTool, "big.go"))}}, ch, nil)
	if st.State != StateRan || st.Found != maxFindingsPerTool {
		t.Errorf("exactly at the cap: %+v", st)
	}
}
