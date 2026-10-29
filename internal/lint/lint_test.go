package lint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeRunner struct {
	mu    sync.Mutex
	execs []core.ExecRequest
	// by argv[0] (for npx: argv[2])
	handlers map[string]func(core.ExecRequest) (*core.ExecResult, error)
}

func (f *fakeRunner) Run(context.Context, core.RunRequest) (*core.RunResult, error) {
	return nil, errors.New("no claude in lint tests")
}
func (f *fakeRunner) Exec(_ context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	f.mu.Lock()
	f.execs = append(f.execs, req)
	f.mu.Unlock()
	name := req.Command[0]
	if name == "npx" {
		name = req.Command[2]
	}
	if h, ok := f.handlers[name]; ok {
		return h(req)
	}
	return &core.ExecResult{ExitCode: 127, Stderr: "exec: \"" + name + "\": executable file not found in $PATH"}, nil
}

func (f *fakeRunner) tools() []string {
	var out []string
	for _, e := range f.execs {
		out = append(out, e.Command[0])
	}
	return out
}

type fakeCheckout struct {
	dir   string
	diff  string
	files []string
	base  map[string]string // files at the base commit
}

func (c *fakeCheckout) Dir() string { return c.dir }
func (c *fakeCheckout) Diff(context.Context, string, []string) (string, error) {
	return c.diff, nil
}
func (c *fakeCheckout) ChangedFiles(context.Context, string, []string) ([]string, error) {
	return nil, nil
}
func (c *fakeCheckout) IsAncestor(context.Context, string) (bool, error) { return true, nil }
func (c *fakeCheckout) ShowFile(_ context.Context, rev, p string) ([]byte, error) {
	if rev == "base" {
		if b, ok := c.base[p]; ok {
			return []byte(b), nil
		}
	}
	return nil, core.ErrNotFound
}
func (c *fakeCheckout) ListFiles(_ context.Context, rev string) ([]string, error) {
	if rev == "base" {
		var out []string
		for p := range c.base {
			out = append(out, p)
		}
		return out, nil
	}
	return c.files, nil
}
func (c *fakeCheckout) Close() error { return nil }

func newCheckout(t *testing.T, diff string, files ...string) *fakeCheckout {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeCheckout{dir: dir, diff: diff, files: files, base: map[string]string{}}
}

const goDiff = `diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -1,4 +1,6 @@
 package main

+import "os"
+
 func main() {
-	println("x")
+	os.Exit(run())
 }
diff --git a/old.go b/old.go
deleted file mode 100644
--- a/old.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package main
-var x = 1
diff --git a/app/util.py b/app/util.py
new file mode 100644
--- /dev/null
+++ b/app/util.py
@@ -0,0 +1,3 @@
+import os
+def f():
+    x = 1
diff --git a/config.yaml b/config.yaml
--- a/config.yaml
+++ b/config.yaml
@@ -10,3 +10,2 @@
 a: 1
-b: 2
 c: 3
`

// ---------------------------------------------------------------------------
// Changed lines
// ---------------------------------------------------------------------------

func TestParseChangedLines(t *testing.T) {
	c := ParseChangedLines(goDiff)
	for _, tc := range []struct {
		path string
		line int
		want bool
	}{
		{"main.go", 1, false}, {"main.go", 3, true}, {"main.go", 4, true}, {"main.go", 5, false},
		{"main.go", 6, true}, {"main.go", 7, false},
		{"app/util.py", 1, true}, {"app/util.py", 3, true}, {"app/util.py", 4, false},
		{"old.go", 2, false}, {"config.yaml", 10, false}, {"config.yaml", 11, false},
	} {
		if got := c.Line(tc.path, tc.line); got != tc.want {
			t.Errorf("%s:%d = %v, want %v", tc.path, tc.line, got, tc.want)
		}
	}
	if got := strings.Join(c.Files(), ","); got != "main.go,app/util.py" {
		t.Errorf("files with added lines: %s (removed-only and deleted files must not count)", got)
	}
	if !c.Any("main.go", 5, 6) || c.Any("main.go", 7, 9) {
		t.Error("Any")
	}
}

func TestParseChangedLinesQuotedPath(t *testing.T) {
	c := ParseChangedLines("diff --git \"a/d\\303\\251j\\303\\240 vu.go\" \"b/d\\303\\251j\\303\\240 vu.go\"\n--- \"a/d\\303\\251j\\303\\240 vu.go\"\n+++ \"b/d\\303\\251j\\303\\240 vu.go\"\n@@ -1 +1 @@\n-a\n+b\n")
	if !c.Line("déjà vu.go", 1) {
		t.Fatalf("quoted path not decoded: %v", c.Files())
	}
}

func TestKeepOnlyChangedLines(t *testing.T) {
	c := ParseChangedLines(goDiff)
	in := []Finding{
		{Tool: "ci:ruff", Path: "app/util.py", Line: 3, Rule: "F841"},
		{Tool: "ci:ruff", Path: "app/util.py", Line: 10, Rule: "E1"},          // not in the diff
		{Tool: "ci:golangci", Path: "main.go", Line: 5, Rule: "unused"},       // context line
		{Tool: "ci:semgrep", Path: "main.go", Line: 1, EndLine: 4, Rule: "r"}, // range covers added line 3
		{Tool: "ci:semgrep", Path: "config.yaml", Line: 11, Rule: "r"},        // removed-only hunk
		{Tool: "ci:semgrep", Path: "old.go", Line: 2, Rule: "r"},              // deleted file
		{Tool: "ci:semgrep", Path: "", Line: 2, Rule: "r"},
	}
	out := keep(in, c)
	if len(out) != 2 || out[0].Rule != "F841" || out[1].Line != 3 || !out[1].InDiff {
		t.Fatalf("kept %+v", out)
	}
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

func TestCommands(t *testing.T) {
	if got := strings.Join(GitleaksCommand("aaa", "bbb"), " "); got != "gitleaks git --log-opts=aaa..bbb --config .reviewbot-lint/gitleaks.toml --gitleaks-ignore-path .reviewbot-lint/ignore --report-format json --report-path - --redact --ignore-gitleaks-allow --no-banner --no-color --log-level error --platform none --exit-code 2 .git" {
		t.Errorf("gitleaks: %s", got)
	}
	for _, bad := range []string{"", "-x", "a b", "a;b", "$(x)"} {
		if validRev(bad) {
			t.Errorf("validRev(%q)", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Parsers (samples derived from the tools' own docs/sources)
// ---------------------------------------------------------------------------

const secretValue = "AKIAZ7Q3XSUPERSECRET42"

// gitleaksSample follows report.Finding of gitleaks v8.30 (JSON reporter,
// testdata/expected/report/json_simple.json) — before redaction, to prove
// the parser drops the value even if --redact were missing.
var gitleaksSample = `[
 {
  "RuleID": "aws-access-token",
  "Description": "Identified a pattern that may indicate AWS credentials, risking unauthorized cloud resource access and data breaches on AWS platforms.",
  "StartLine": 3,
  "EndLine": 3,
  "StartColumn": 14,
  "EndColumn": 33,
  "Match": "aws_key = \"` + secretValue + `\"",
  "Secret": "` + secretValue + `",
  "File": "main.go",
  "SymlinkFile": "",
  "Commit": "0123456789abcdef0123456789abcdef01234567",
  "Link": "https://gitlab.example.com/g/p/-/blob/0123/main.go#L3",
  "Entropy": 3.9,
  "Author": "Mallory",
  "Email": "mallory@example.com",
  "Date": "2026-10-20T10:00:00Z",
  "Message": "add key ` + secretValue + `",
  "Tags": [],
  "Fingerprint": "0123456789abcdef:main.go:aws-access-token:3"
 }
]`

func TestParseGitleaksNeverKeepsSecret(t *testing.T) {
	fs, err := ParseGitleaks([]byte(gitleaksSample))
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	f := fs[0]
	if f.Rule != "aws-access-token" || f.Path != "main.go" || f.Line != 3 || f.Severity != core.SeverityCritical || f.Commit != "0123456789ab" || !f.Secret() {
		t.Errorf("%+v", f)
	}
	if dump := fmt.Sprintf("%#v", fs); strings.Contains(dump, secretValue) || strings.Contains(dump, "Mallory") || strings.Contains(dump, "mallory@") {
		t.Fatalf("secret material in parsed finding: %s", dump)
	}
	if fs, err := ParseGitleaks([]byte("[]\n")); err != nil || len(fs) != 0 {
		t.Errorf("empty: %v %v", fs, err)
	}
	if _, err := ParseGitleaks([]byte("not json")); err == nil {
		t.Error("garbage must be an error")
	}
}

func TestIDIgnoresLineNumbersAndCase(t *testing.T) {
	a := ID("ci:semgrep", "r", "a.go", "Variable  x on line 10")
	b := ID("ci:semgrep", "r", "a.go", "variable x on line 42")
	if a != b || len(a) != 12 {
		t.Errorf("%s %s", a, b)
	}
	if a == ID("ci:semgrep", "r2", "a.go", "variable x on line 10") || a == ID("ci:ruff", "r", "a.go", "variable x on line 10") {
		t.Error("tool and rule must be part of the id")
	}
}

// ---------------------------------------------------------------------------
// Check: detection, missing tools, filtering, redaction
// ---------------------------------------------------------------------------

func TestCheckMissingToolsAreUnavailable(t *testing.T) {
	co := newCheckout(t, goDiff, "go.mod", "main.go", "app/util.py", "config.yaml")
	r := &fakeRunner{}
	res, err := (&Checker{Runner: r}).Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head", Config: Config{Tools: AllTools}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("%+v", res.Findings)
	}
	st, ok := res.Status(ToolGitleaks)
	if !ok || st.State != StateUnavailable || !strings.Contains(st.Reason, "не установлен") || res.ToolOK(ToolGitleaks) {
		t.Errorf("gitleaks: %+v", st)
	}
	for _, e := range r.execs {
		if e.Dir != co.dir || len(e.Env) > 0 {
			t.Errorf("exec %+v", e)
		}
	}
	if _, err := os.Stat(filepath.Join(co.dir, ScratchDir)); !os.IsNotExist(err) {
		t.Error("scratch dir must be removed")
	}
}

// gitleaks is the one tool the bot still runs itself: findings come out
// critical whatever the overrides say, the value never reaches the result,
// and unknown tool names in the config (removed linters) are not run.
func TestCheckGitleaksRunsAndRedacts(t *testing.T) {
	co := newCheckout(t, goDiff, "go.mod", "main.go", "app/util.py", "config.yaml")
	r := &fakeRunner{handlers: map[string]func(core.ExecRequest) (*core.ExecResult, error){
		"gitleaks": func(req core.ExecRequest) (*core.ExecResult, error) {
			if _, err := os.Stat(filepath.Join(co.dir, ScratchDir, "gitleaks.toml")); err != nil {
				t.Errorf("gitleaks config not prepared: %v", err)
			}
			return &core.ExecResult{ExitCode: 2, Stdout: gitleaksSample, Stderr: "leaks found: 1"}, nil
		},
	}}
	res, err := (&Checker{Runner: r}).Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head", Config: Config{
		Tools:             []string{"semgrep", ToolGitleaks, "ruff"},
		SeverityOverrides: map[string]core.Severity{ToolGitleaks: core.SeverityNit},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if dump := fmt.Sprintf("%#v", res); strings.Contains(dump, secretValue) {
		t.Fatalf("secret value leaked into the result: %s", dump)
	}
	if len(res.Findings) != 1 || res.Findings[0].Severity != core.SeverityCritical || !res.Findings[0].InDiff || !res.ToolOK(ToolGitleaks) {
		t.Fatalf("%+v %+v", res.Findings, res.Tools)
	}
	if len(res.Tools) != 1 || len(r.execs) != 1 || r.execs[0].Command[0] != ToolGitleaks {
		t.Errorf("only gitleaks runs: %+v %v", res.Tools, r.tools())
	}
}

func TestCheckGitleaksErrors(t *testing.T) {
	for name, h := range map[string]func(core.ExecRequest) (*core.ExecResult, error){
		"gitleaks завершился с кодом 1": func(core.ExecRequest) (*core.ExecResult, error) {
			return &core.ExecResult{ExitCode: 1, Stderr: "ERR failed to scan Git repository"}, nil
		},
		"превышено время выполнения": func(core.ExecRequest) (*core.ExecResult, error) {
			return nil, fmt.Errorf("exec aborted: %w", context.DeadlineExceeded)
		},
		"непонятный вывод gitleaks": func(core.ExecRequest) (*core.ExecResult, error) {
			return &core.ExecResult{ExitCode: 0, Stdout: "not json"}, nil
		},
	} {
		co := newCheckout(t, goDiff, "main.go")
		res, _ := gitleaksOnly(t, co, h, "base", "head")
		st, _ := res.Status(ToolGitleaks)
		if st.State != StateFailed || !strings.Contains(st.Reason, name) || res.ToolOK(ToolGitleaks) {
			t.Errorf("%s: %+v", name, st)
		}
	}
}

func TestCheckGitleaksUsesTargetBranchConfig(t *testing.T) {
	co := newCheckout(t, goDiff, "main.go", ".gitleaks.toml")
	co.base[".gitleaks.toml"] = "[extend]\nuseDefault = true\n[[rules]]\nid = \"corp-token\"\nregex = '''corp_[a-z0-9]{32}'''\n"
	co.base[".gitleaksignore"] = "abc:main.go:aws-access-token:3\n"
	var cfg, ign string
	r := &fakeRunner{handlers: map[string]func(core.ExecRequest) (*core.ExecResult, error){
		"gitleaks": func(core.ExecRequest) (*core.ExecResult, error) {
			b, _ := os.ReadFile(filepath.Join(co.dir, ScratchDir, "gitleaks.toml"))
			cfg = string(b)
			b, _ = os.ReadFile(filepath.Join(co.dir, ScratchDir, "ignore", ".gitleaksignore"))
			ign = string(b)
			return &core.ExecResult{ExitCode: 0, Stdout: "[]"}, nil
		},
	}}
	res, err := (&Checker{Runner: r}).Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head", Config: Config{Tools: []string{ToolGitleaks}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, "corp-token") || !strings.Contains(ign, "aws-access-token") {
		t.Errorf("config/ignore must come from the target branch: %q %q", cfg, ign)
	}
	if !res.ToolOK(ToolGitleaks) {
		t.Errorf("%+v", res.Tools)
	}
	b, _ := os.ReadFile(filepath.Join(co.dir, ".git", "info", "exclude"))
	if !strings.Contains(string(b), "/"+ScratchDir+"/") {
		t.Error("scratch dir must be git-excluded")
	}
}

func TestCheckGitleaksSecretOutsideFinalDiff(t *testing.T) {
	// The secret was added in one commit of the MR and removed later: still
	// reported (it stays in the history), but not inline.
	co := newCheckout(t, goDiff, "main.go")
	r := &fakeRunner{handlers: map[string]func(core.ExecRequest) (*core.ExecResult, error){
		"gitleaks": func(core.ExecRequest) (*core.ExecResult, error) {
			return &core.ExecResult{ExitCode: 2, Stdout: strings.Replace(gitleaksSample, `"StartLine": 3`, `"StartLine": 40`, 1)}, nil
		},
	}}
	res, _ := (&Checker{Runner: r}).Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head", Config: Config{Tools: []string{ToolGitleaks}}})
	if len(res.Findings) != 1 || res.Findings[0].InDiff || res.Findings[0].Line != 40 {
		t.Fatalf("%+v", res.Findings)
	}
}

func TestGitleaksIgnoreNote(t *testing.T) {
	co := newCheckout(t, "diff --git a/.gitleaksignore b/.gitleaksignore\n--- a/.gitleaksignore\n+++ b/.gitleaksignore\n@@ -0,0 +1 @@\n+x:y:z:1\n", ".gitleaksignore")
	res, _ := (&Checker{Runner: &fakeRunner{}}).Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head", Config: Config{Tools: []string{ToolGitleaks}}})
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], ".gitleaksignore") {
		t.Errorf("%v", res.Notes)
	}
}

// An MR that commits a symlink at .reviewbot-lint (pointing outside the
// clone) must not make the bot write its config files there.
func TestScratchDirMaliciousSymlink(t *testing.T) {
	co := newCheckout(t, goDiff, "main.go")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(co.dir, ScratchDir)); err != nil {
		t.Fatal(err)
	}
	var sawConfig bool
	r := &fakeRunner{handlers: map[string]func(core.ExecRequest) (*core.ExecResult, error){
		"gitleaks": func(core.ExecRequest) (*core.ExecResult, error) {
			fi, err := os.Lstat(filepath.Join(co.dir, ScratchDir))
			sawConfig = err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0
			_, err = os.Stat(filepath.Join(co.dir, ScratchDir, "gitleaks.toml"))
			sawConfig = sawConfig && err == nil
			return &core.ExecResult{ExitCode: 0, Stdout: "[]"}, nil
		},
	}}
	if _, err := (&Checker{Runner: r}).Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head",
		Config: Config{Tools: []string{ToolGitleaks}}}); err != nil {
		t.Fatal(err)
	}
	if !sawConfig {
		t.Error("the scratch dir must be a real directory inside the clone")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("wrote outside the clone: %v", ents)
	}
}
