package lint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// ---------------------------------------------------------------------------
// Blocking-relevant classification, ids, severities
// ---------------------------------------------------------------------------

func TestIsLinterNeverForScanners(t *testing.T) {
	for _, tool := range []string{"ci:phpstan", "ci:ruff", "ci:x"} {
		if !IsLinter(tool) || !IsCI(tool) {
			t.Errorf("%s is a linter (never blocks unless marked)", tool)
		}
	}
	for _, tool := range []string{ToolGitleaks, "", "ci", "custom:arch", "semgrep"} {
		if IsLinter(tool) {
			t.Errorf("%s must not be a linter: its findings follow the severity gate", tool)
		}
	}
}

// Two secrets of one rule in one file are separate threads; a non-secret
// result keeps its id when it moves.
func TestFindingIDGitleaksVsOthers(t *testing.T) {
	g := Finding{Tool: ToolGitleaks, Rule: "aws", Path: "a.env", Line: 3, Commit: "c1", Message: "AWS key"}
	g2, g3 := g, g
	g2.Line = 4
	g3.Commit = "c2"
	if g.ID() == g2.ID() || g.ID() == g3.ID() {
		t.Errorf("gitleaks ids must depend on commit and line: %s %s %s", g.ID(), g2.ID(), g3.ID())
	}
	if g.ID() != (Finding{Tool: ToolGitleaks, Rule: "aws", Path: "a.env", Line: 3, Commit: "c1", Message: "other text"}).ID() {
		t.Error("gitleaks id must not depend on the message")
	}
	s := Finding{Tool: "ci:semgrep", Rule: "r", Path: "a.go", Line: 3, Message: "bad call"}
	s2 := s
	s2.Line, s2.Commit = 90, "zz"
	if s.ID() != s2.ID() {
		t.Error("a moved semgrep result keeps its id")
	}
	s2.Message = "other call"
	if s.ID() == s2.ID() {
		t.Error("the message is part of a non-secret id")
	}
}

func TestApplySeverityAfterSecret(t *testing.T) {
	fs := applySeverity([]Finding{
		{Tool: ToolGitleaks, Rule: "aws", Severity: core.SeverityMinor},
		{Tool: "ci:semgrep", Rule: "r1", Severity: core.SeverityMajor},
		{Tool: "ci:semgrep", Rule: "r2", Severity: core.SeverityMajor},
		{Tool: "ci:ruff", Rule: "E1"},
	}, map[string]core.Severity{"ci:semgrep": core.SeverityNit, "ci:semgrep:r2": core.SeverityCritical, ToolGitleaks: core.SeverityNit})
	got := []core.Severity{fs[0].Severity, fs[1].Severity, fs[2].Severity, fs[3].Severity}
	want := []core.Severity{core.SeverityCritical, core.SeverityNit, core.SeverityCritical, core.SeverityMinor}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d: %s, want %s", i, got[i], want[i])
		}
	}
}

func TestSortFindings(t *testing.T) {
	fs := []Finding{
		{Tool: "ci:ruff", Path: "b.py", Line: 1, Severity: core.SeverityNit},
		{Tool: "ci:semgrep", Path: "b.go", Line: 9, Severity: core.SeverityMajor},
		{Tool: "ci:semgrep", Path: "a.go", Line: 9, Severity: core.SeverityCritical},
		{Tool: "ci:ruff", Path: "a.py", Line: 7, Severity: core.SeverityMinor},
		{Tool: "ci:ruff", Path: "a.py", Line: 2, Severity: core.SeverityMinor},
		{Tool: ToolGitleaks, Path: "z.env", Line: 5, Severity: core.SeverityCritical},
		{Tool: "ci:semgrep", Path: "a.go", Line: 1, Severity: core.SeverityMajor},
	}
	sortFindings(fs)
	var got []string
	for _, f := range fs {
		got = append(got, fmt.Sprintf("%s:%d", f.Path, f.Line))
	}
	want := "z.env:5 a.go:9 a.go:1 b.go:9 a.py:2 a.py:7 b.py:1"
	if strings.Join(got, " ") != want {
		t.Errorf("order %v, want %s", got, want)
	}
}

// ---------------------------------------------------------------------------
// Changed lines filter
// ---------------------------------------------------------------------------

func TestKeepMultiLineAnchoring(t *testing.T) {
	c := ParseChangedLines(goDiff) // main.go: 3, 4, 6 added; app/util.py: 1-3
	for _, tc := range []struct {
		name     string
		f        Finding
		wantLine int // 0 = dropped
	}{
		{"line 0 with a range", Finding{Path: "main.go", Line: 0, EndLine: 3}, 0},
		{"range after an added line", Finding{Path: "app/util.py", Line: 4, EndLine: 5}, 0},
		{"next line added", Finding{Path: "main.go", Line: 2, EndLine: 3}, 3},
		{"only the end line added", Finding{Path: "main.go", Line: 5, EndLine: 6}, 6},
		{"single context line", Finding{Path: "main.go", Line: 5, EndLine: 5}, 0},
		{"end before start", Finding{Path: "main.go", Line: 3, EndLine: 1}, 3},
	} {
		out := keep([]Finding{tc.f}, c)
		switch {
		case tc.wantLine == 0 && len(out) != 0:
			t.Errorf("%s: kept %+v", tc.name, out)
		case tc.wantLine != 0 && (len(out) != 1 || out[0].Line != tc.wantLine || !out[0].InDiff):
			t.Errorf("%s: %+v, want line %d", tc.name, out, tc.wantLine)
		}
	}
	far := &Changed{lines: map[string]map[int]bool{"x.go": {202: true}}}
	if out := keep([]Finding{{Path: "x.go", Line: 1, EndLine: 500}}, far); len(out) != 0 {
		t.Errorf("anchoring looks at most 200 lines into a range: %+v", out)
	}
	near := &Changed{lines: map[string]map[int]bool{"x.go": {201: true}}}
	if out := keep([]Finding{{Path: "x.go", Line: 1, EndLine: 500}}, near); len(out) != 1 || out[0].Line != 201 {
		t.Errorf("line+200 is still searched: %+v", out)
	}
}

func TestValidRev(t *testing.T) {
	for s, want := range map[string]bool{
		"abc123": true, "0123456789abcdefABCDEF": true, "main": true, "feature/x-y_z.1": true, "HEAD~1": true, "a^": true,
		"az": true, "AZ": true, "09": true,
		"": false, "-x": false, "--output=/tmp/x": false, "a b": false, "a;b": false, "$(id)": false, "a\nb": false,
		"a:b": false, "a@{1}": false, strings.Repeat("a", 201): false,
	} {
		if got := validRev(s); got != want {
			t.Errorf("validRev(%q) = %v, want %v", s, got, want)
		}
	}
	if !validRev(strings.Repeat("a", 200)) {
		t.Error("200 characters are fine")
	}
}

func TestRelPathThroughSymlinkedCheckout(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "clone")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	e := &runEnv{dir: link}
	for in, want := range map[string]string{
		real + "/src/A.php": "src/A.php",
		link + "/src/B.php": "src/B.php",
		"/work/./c/d.go":    "c/d.go",
		"././e.go":          "e.go",
		`dir\win.go`:        "dir/win.go",
	} {
		if got := e.relPath(in); got != want {
			t.Errorf("relPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func bigDiff(path string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, path, path, n)
	for i := 0; i < n; i++ {
		b.WriteString("+x\n")
	}
	return b.String()
}

func sarifWith(n int, path string) string {
	var rs []string
	for i := 1; i <= n; i++ {
		rs = append(rs, fmt.Sprintf(`{"ruleId":"r","level":"error","message":{"text":"m %d"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":%q},"region":{"startLine":%d}}}]}`, i, path, i))
	}
	return `{"runs":[{"results":[` + strings.Join(rs, ",") + `]}]}`
}

// ---------------------------------------------------------------------------
// gitleaks
// ---------------------------------------------------------------------------

func gitleaksOnly(t *testing.T, co *fakeCheckout, h func(core.ExecRequest) (*core.ExecResult, error), base, head string) (*Result, *fakeRunner) {
	t.Helper()
	r := &fakeRunner{handlers: map[string]func(core.ExecRequest) (*core.ExecResult, error){"gitleaks": h}}
	res, err := (&Checker{Runner: r}).Check(context.Background(), Request{Checkout: co, Base: base, Head: head, Config: Config{Tools: []string{ToolGitleaks}}})
	if err != nil {
		t.Fatal(err)
	}
	return res, r
}

func TestGitleaksRejectsOptionLikeRevs(t *testing.T) {
	for _, revs := range [][2]string{{"--output=/tmp/x", "head"}, {"base", "head --all"}, {"", "head"}} {
		co := newCheckout(t, goDiff, "main.go")
		res, r := gitleaksOnly(t, co, func(core.ExecRequest) (*core.ExecResult, error) {
			return &core.ExecResult{Stdout: "[]"}, nil
		}, revs[0], revs[1])
		if len(r.execs) != 0 {
			t.Errorf("%q: gitleaks must not run: %v", revs, r.execs)
		}
		if st, _ := res.Status(ToolGitleaks); st.State != StateFailed || res.ToolOK(ToolGitleaks) {
			t.Errorf("%q: %+v", revs, st)
		}
	}
}

// The MR may commit .reviewbot-lint/ itself: a planted ignore file must not
// survive into the gitleaks run (the scratch dir is recreated).
func TestGitleaksPlantedScratchIgnoreIsDropped(t *testing.T) {
	co := newCheckout(t, goDiff, "main.go")
	planted := filepath.Join(co.dir, ScratchDir, "ignore", ".gitleaksignore")
	if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planted, []byte("0123456789ab:main.go:aws-access-token:3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sawPlanted bool
	_, _ = gitleaksOnly(t, co, func(core.ExecRequest) (*core.ExecResult, error) {
		_, err := os.Stat(planted)
		sawPlanted = err == nil
		return &core.ExecResult{Stdout: "[]"}, nil
	}, "base", "head")
	if sawPlanted {
		t.Error("a .gitleaksignore committed in the scratch dir must not reach gitleaks")
	}
}

func TestGitleaksEmptyTargetConfigUsesDefault(t *testing.T) {
	co := newCheckout(t, goDiff, "main.go")
	co.base[".gitleaks.toml"] = ""
	var cfg string
	_, _ = gitleaksOnly(t, co, func(core.ExecRequest) (*core.ExecResult, error) {
		b, _ := os.ReadFile(filepath.Join(co.dir, ScratchDir, "gitleaks.toml"))
		cfg = string(b)
		return &core.ExecResult{Stdout: "[]"}, nil
	}, "base", "head")
	if cfg != gitleaksDefaultConfig {
		t.Errorf("an empty target-branch config must not disable the rules: %q", cfg)
	}
}

func TestGitleaksDedupesAndKeepsDistinctLines(t *testing.T) {
	co := newCheckout(t, goDiff, "main.go")
	out := `[{"RuleID":"aws","File":"main.go","StartLine":3,"Commit":"c1"},{"RuleID":"aws","File":"main.go","StartLine":3,"Commit":"c1"},
	{"RuleID":"aws","File":"main.go","StartLine":4,"Commit":"c1"}]`
	res, _ := gitleaksOnly(t, co, func(core.ExecRequest) (*core.ExecResult, error) {
		return &core.ExecResult{ExitCode: gitleaksLeakExit, Stdout: out}, nil
	}, "base", "head")
	if len(res.Findings) != 2 || res.Findings[0].Line != 3 || res.Findings[1].Line != 4 {
		t.Fatalf("findings %+v", res.Findings)
	}
	if st, _ := res.Status(ToolGitleaks); st.Total != 3 || st.Found != 2 {
		t.Errorf("status %+v", st)
	}
}

func TestParseGitleaksEdgeCases(t *testing.T) {
	for _, in := range []string{"", "  \n", "null", " null\n"} {
		fs, err := ParseGitleaks([]byte(in))
		if err != nil || len(fs) != 0 {
			t.Errorf("%q: no leaks, got %v %v", in, fs, err)
		}
	}
	fs, err := ParseGitleaks([]byte(`[{"RuleID":" ","Description":"","File":"a","StartLine":1,"Commit":"0123456789abcdef"},{"RuleID":"r","Description":" Custom text ","File":"b","StartLine":2}]`))
	if err != nil || len(fs) != 2 {
		t.Fatalf("%v %v", fs, err)
	}
	if fs[0].Rule != "secret" || fs[0].Message != "секрет (secret)" || fs[0].Commit != "0123456789ab" {
		t.Errorf("defaults %+v", fs[0])
	}
	if fs[1].Message != "Custom text" {
		t.Errorf("description %+v", fs[1])
	}
}

// ---------------------------------------------------------------------------
// Report parsers
// ---------------------------------------------------------------------------

func TestParseSARIFEdgeCases(t *testing.T) {
	loc := func(uri string, start, end int) string {
		return fmt.Sprintf(`"locations":[{"physicalLocation":{"artifactLocation":{"uri":%q},"region":{"startLine":%d,"endLine":%d}}}]`, uri, start, end)
	}
	in := `{"runs":[{"results":[
	  {"ruleId":"noloc","message":{"text":"x"}},
	  {"ruleId":"zero",` + loc("a.go", 0, 0) + `},
	  {"ruleId":"nopath",` + loc("", 3, 0) + `},
	  {"ruleId":"range",` + loc("a.go", 3, 7) + `},
	  {"ruleId":"back",` + loc("a.go", 5, 2) + `},
	  {"ruleId":"open","kind":"open",` + loc("a.go", 1, 0) + `},
	  {"ruleId":"fail","kind":"fail","level":"warning",` + loc("a.go", 1, 0) + `}
	]},{"results":[{"ruleId":"second",` + loc("b.go", 1, 0) + `}]}]}`
	fs, err := ParseReport([]byte(in), FormatSARIF, "ci:x")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		got = append(got, fmt.Sprintf("%s:%d-%d", f.Rule, f.Line, f.EndLine))
	}
	if strings.Join(got, " ") != "range:3-7 back:5-0 fail:1-0 second:1-0" {
		t.Errorf("results %v", got)
	}
	bad := `{"runs":[{"results":[{"ruleId":"r","locations":[{"physicalLocation":{"artifactLocation":{"uri":"a.go"},"region":{"startLine":"3"}}}]}]}]}`
	if fs, err := ParseReport([]byte(bad), FormatSARIF, "ci:x"); err == nil {
		t.Errorf("mistyped SARIF must fail, got %+v", fs)
	}
	if sarifSeverity("NONE") != core.SeverityNit || sarifSeverity("Error") != core.SeverityMajor || sarifSeverity("") != core.SeverityMinor {
		t.Error("sarif levels")
	}
}

func TestParseCheckstyleEdgeCases(t *testing.T) {
	in := `<checkstyle>
	<file name=""><error line="3" severity="error" message="no file" source="s"/></file>
	<file name="a.go">
	  <error line="0" severity="error" message="no line" source="s"/>
	  <error line="4" severity="info" message="x &nbsp; y" source="s"/>
	</file>
	<file name="b.go"><error line="1" severity="ignore" message="m" source="t"/></file>
	</checkstyle>`
	fs, err := ParseReport([]byte(in), "", "ci:x")
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 || fs[0].Path != "a.go" || fs[0].Line != 4 || fs[0].Severity != core.SeverityNit || fs[1].Path != "b.go" {
		t.Errorf("findings %+v", fs)
	}
}

func TestIsHTTPURLLength(t *testing.T) {
	u := "https://e.example/" + strings.Repeat("a", 300-len("https://e.example/"))
	if !isHTTPURL(u) || isHTTPURL(u+"a") {
		t.Errorf("300 characters allowed, 301 not")
	}
	for _, bad := range []string{"javascript:alert(1)", "https://x/a b", "https://x/(y)", "ftp://x"} {
		if isHTTPURL(bad) {
			t.Errorf("%q", bad)
		}
	}
}

func TestGitleaksIgnoreNoteOnlyWithGitleaks(t *testing.T) {
	co := newCheckout(t, "diff --git a/.gitleaksignore b/.gitleaksignore\n--- a/.gitleaksignore\n+++ b/.gitleaksignore\n@@ -0,0 +1 @@\n+x:y:z:1\n", ".gitleaksignore")
	res, _ := (&Checker{Runner: &fakeRunner{}}).Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head", Config: Config{Tools: []string{"todo-none"}}})
	if len(res.Notes) != 0 {
		t.Errorf("gitleaks is not run: no note, got %v", res.Notes)
	}
}
