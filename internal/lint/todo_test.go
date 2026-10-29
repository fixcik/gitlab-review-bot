package lint

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// todoCO serves files at the "head" revision.
type todoCO struct{ files map[string]string }

func (c *todoCO) Dir() string                                                      { return "/work" }
func (c *todoCO) Diff(context.Context, string, []string) (string, error)           { return "", nil }
func (c *todoCO) ChangedFiles(context.Context, string, []string) ([]string, error) { return nil, nil }
func (c *todoCO) IsAncestor(context.Context, string) (bool, error)                 { return true, nil }
func (c *todoCO) ListFiles(context.Context, string) ([]string, error)              { return nil, nil }
func (c *todoCO) Close() error                                                     { return nil }
func (c *todoCO) ShowFile(_ context.Context, rev, p string) ([]byte, error) {
	if s, ok := c.files[p]; ok && rev == "head" {
		return []byte(s), nil
	}
	return nil, core.ErrNotFound
}

// addedDiff is the diff of a new file with these lines.
func addedDiff(p string, lines ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", p, p, p, len(lines))
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func defTodo() TodoConfig {
	return TodoConfig{Keywords: []string{"TODO", "FIXME", "XXX", "HACK"}, RequireTask: true, Severity: core.SeverityMinor}
}

// todoNew runs the check on new files (all lines added, readable at head).
func todoNew(t *testing.T, cfg TodoConfig, files map[string][]string) ([]Finding, ToolStatus) {
	t.Helper()
	co := &todoCO{files: map[string]string{}}
	var diff strings.Builder
	for p, lines := range files {
		co.files[p] = strings.Join(lines, "\n") + "\n"
		diff.WriteString(addedDiff(p, lines...))
	}
	return CheckTodo(context.Background(), TodoRequest{Checkout: co, Head: "head", Diff: diff.String(), Config: cfg})
}

// marks renders findings as "line:rule".
func marks(fs []Finding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%d:%s", f.Line, f.Rule))
	}
	return out
}

func TestTodoDetectionAcrossLanguages(t *testing.T) {
	for _, tc := range []struct {
		path  string
		lines []string
		want  []string
	}{
		{"x.go", []string{
			"package x",
			"// TODO: rework cache",
			"// TODO(PROJ-12): rework",
			`var s = "TODO list"`,
			"var c = context.TODO()",
			"x := 1 // FIXME later",
			"/* HACK: temporary */",
			"/*",
			" * XXX: multi",
			" */",
			"raw := `",
			"TODO in a raw string",
			"`",
			"// TODOS are fine, todo lowercase too",
			`s := "// TODO: not a comment"`,
			`r := '"' // TODO: after a rune`,
			"/* a */ x() /* FIXME: b */",
			`var u = "http://x" // TODO see #42`,
		}, []string{"2:TODO", "6:FIXME", "7:HACK", "9:XXX", "16:TODO", "17:FIXME"}},
		{"a.py", []string{
			"# TODO: add retries",
			`s = "# TODO not a comment"`,
			"def f():",
			`    """TODO: describe the function."""`,
			`    x = """TODO list"""`,
			`    """`,
			"    FIXME: multi-line docstring",
			`    """`,
			`    y = """`,
			"TODO inside a string",
			`"""`,
			"    return x  # XXX: hack",
		}, []string{"1:TODO", "4:TODO", "7:FIXME", "12:XXX"}},
		{"a.php", []string{
			"<?php",
			"# TODO: legacy hash comment",
			"// FIXME: slash comment",
			"$s = 'TODO: not a comment'; // ok",
			"/**",
			" * HACK: docblock line",
			" * @todo lowercase phpdoc",
			" */",
			`$t = "it's # TODO";`,
		}, []string{"2:TODO", "3:FIXME", "6:HACK"}},
		{"a.ts", []string{
			"const q = `",
			"TODO in a template literal",
			"`; // TODO: after the template",
			"const a = '// TODO'",
			"/* FIXME */ const b = 1",
			"const url = 'http://x/TODO' // XXX tidy",
		}, []string{"3:TODO", "5:FIXME", "6:XXX"}},
		{"m.sql", []string{
			"-- TODO: add an index",
			"SELECT 'TODO -- x' AS s;",
			"/* FIXME: lock */",
			"SELECT 1; -- HACK",
		}, []string{"1:TODO", "3:FIXME", "4:HACK"}},
		{"c.yaml", []string{
			"key: value # TODO: tune",
			`other: "a # TODO"`,
			"url: http://x#TODO",
			"# FIXME(OPS-1): ok",
			"msg: it's fine # XXX later",
		}, []string{"1:TODO", "5:XXX"}},
		{"p.html", []string{
			"<!-- TODO: footer -->",
			"<p>TODO list</p>",
			"<!--",
			"  FIXME: multi-line comment",
			"-->",
		}, []string{"1:TODO", "4:FIXME"}},
		{"t.html.twig", []string{
			"{# TODO: translate #}",
			"{{ 'TODO' }}",
			"<!-- HACK: inline -->",
		}, []string{"1:TODO", "3:HACK"}},
		{"s.sh", []string{
			`echo "TODO" # FIXME: quote`,
			"echo ${#arr} TODO",
			"# TODO #12",
		}, []string{"1:FIXME"}},
		{"README.md", []string{
			"TODO: write docs",
			"<!-- TODO: hidden -->",
		}, []string{"2:TODO"}},
		{"C.vue", []string{
			"<template>",
			`  <a href="http://x/TODO">TODO</a>`,
			"  <!-- TODO: i18n -->",
			"</template>",
			"<script>",
			"const x = 1 // FIXME later",
			"</script>",
		}, []string{"3:TODO", "6:FIXME"}},
		{"Dockerfile", []string{"# TODO: pin the version", "RUN echo TODO"}, []string{"1:TODO"}},
		{"conf/app.ini", []string{"; TODO: tune", "name = TODO"}, []string{"1:TODO"}},
		{"a.lua", []string{"-- TODO: x", `print("-- TODO")`}, []string{"1:TODO"}},
		{"a.erl", []string{"% TODO: x"}, []string{"1:TODO"}},
		{"a.json", []string{`{"TODO": "// TODO"}`}, nil},
		{"notes.txt", []string{"# TODO: x"}, nil},
		{"x.unknownext", []string{"// TODO: x", "a TODO b"}, []string{"1:TODO"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			fs, st := todoNew(t, defTodo(), map[string][]string{tc.path: tc.lines})
			if st.State != StateRan {
				t.Fatalf("status %+v", st)
			}
			want := tc.want
			if want == nil {
				want = []string{}
			}
			if got := marks(fs); !reflect.DeepEqual(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
			for _, f := range fs {
				if f.Tool != ToolTodo || !f.InDiff || f.Path != tc.path || f.Todo == nil || f.Severity != core.SeverityMinor {
					t.Errorf("bad finding %+v", f)
				}
			}
		})
	}
}

func TestTodoTaskReferences(t *testing.T) {
	for _, tc := range []struct {
		line     string
		projects []string
		flagged  bool
	}{
		{"// TODO(PROJ-123): x", nil, false},
		{"// TODO(PROJ-123): x", []string{"PROJ"}, false},
		{"// TODO(PROJ-123): x", []string{"OPS"}, true},
		{"// TODO PROJ-123 x", nil, false},
		{"// TODO: OLD_PROJ-1", []string{"PROJ"}, false},
		{"// FIXME: see https://jira.example.com/browse/PROJ-5", []string{"OPS"}, false},
		{"// FIXME: https://jira.example.com/secure/RapidBoard.jspa?selectedIssue=PROJ-5", []string{"OPS"}, false},
		{"// TODO: https://gitlab.example.com/g/p/-/issues/7", nil, false},
		{"// TODO: https://github.com/o/r/issues/7", nil, false},
		{"// TODO: see https://example.com/docs/PROJ-1", []string{"OPS"}, true},
		{"// TODO #12", nil, false},
		{"// TODO(group/project#12)", nil, false},
		{"// TODO: project#12", nil, false},
		{"// TODO: support UTF-8", nil, true},
		{"// TODO: SHA-256 everywhere", nil, true},
		{"// TODO-123 fix", nil, true},
		{"// TODO: color #fff", nil, true},
		{"// TODO(alice): x", nil, true},
		{"// TODO: proj-1 lowercase", []string{"PROJ"}, true},
		{"// HACK: &#123; entity", nil, true},
		{"// TODO: x // PROJ-7", []string{"PROJ"}, false},
	} {
		cfg := defTodo()
		cfg.ProjectKeys = tc.projects
		fs, _ := todoNew(t, cfg, map[string][]string{"a.go": {tc.line}})
		if got := len(fs) == 1; got != tc.flagged {
			t.Errorf("%q (projects %v): flagged=%v, want %v", tc.line, tc.projects, got, tc.flagged)
		}
	}
}

func TestTodoContinuationLines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		lines []string
		want  []string
	}{
		{"next comment line", "a.go", []string{"// TODO: rework this", "// see PROJ-9"}, []string{}},
		{"code is not a continuation", "a.go", []string{"// TODO: rework", "foo() // PROJ-1"}, []string{"1:TODO"}},
		{"next marker has its own key", "a.go", []string{"// TODO: a", "// FIXME PROJ-1"}, []string{"1:TODO"}},
		{"docblock", "a.php", []string{"/**", " * TODO: rework", " * PROJ-9", " */"}, []string{}},
		{"blank line breaks it", "a.py", []string{"# TODO: x", "", "# PROJ-1"}, []string{"1:TODO"}},
		{"only the next line", "a.py", []string{"# TODO: x", "# more", "# PROJ-1"}, []string{"1:TODO"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, _ := todoNew(t, defTodo(), map[string][]string{tc.path: tc.lines})
			if got := marks(fs); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTodoOnlyAddedLines(t *testing.T) {
	head := "package x\n/*\n TODO: inside an old block comment\n*/\n// TODO: old\n// FIXME: new\n"
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,5 +1,6 @@\n package x\n /*\n-old\n+ TODO: inside an old block comment\n */\n // TODO: old\n+// FIXME: new\n"
	co := &todoCO{files: map[string]string{"x.go": head}}
	fs, st := CheckTodo(context.Background(), TodoRequest{Checkout: co, Head: "head", Diff: diff, Config: defTodo()})
	if got := marks(fs); st.State != StateRan || !reflect.DeepEqual(got, []string{"3:TODO", "6:FIXME"}) {
		t.Fatalf("got %v %+v", got, st)
	}
	if fs[0].Message != "TODO без задачи — «TODO: inside an old block comment»" {
		t.Errorf("message %q", fs[0].Message)
	}
	// The file cannot be read at the head: only the diff lines are scanned
	// (the unchanged "/*" is a context line, so the block is still known).
	fs, _ = CheckTodo(context.Background(), TodoRequest{Diff: diff, Config: defTodo()})
	if got := marks(fs); !reflect.DeepEqual(got, []string{"3:TODO", "6:FIXME"}) {
		t.Fatalf("diff fallback: %v", got)
	}
	// A hunk inside a docblock whose start the diff does not show: "* "
	// lines are taken for docblock lines (only when the lines before are
	// not known).
	doc := "diff --git a/A.java b/A.java\n--- a/A.java\n+++ b/A.java\n@@ -20,2 +20,3 @@\n  * Does x.\n+ * FIXME: and y\n  */\n"
	fs, _ = CheckTodo(context.Background(), TodoRequest{Diff: doc, Config: defTodo()})
	if got := marks(fs); !reflect.DeepEqual(got, []string{"21:FIXME"}) {
		t.Fatalf("docblock in the diff fallback: %v", got)
	}
	// Nothing added: not applicable.
	_, st = CheckTodo(context.Background(), TodoRequest{Diff: "", Config: defTodo()})
	if st.State != StateNotApplicable {
		t.Fatalf("empty diff: %+v", st)
	}
}

func TestTodoIgnoreAndGenerated(t *testing.T) {
	cfg := defTodo()
	cfg.Ignore = []string{"vendor/**", "node_modules/**", "**/*.min.js", "legacy/**", "*.pb.go"}
	todo := []string{"// TODO: x"}
	fs, _ := todoNew(t, cfg, map[string][]string{
		"vendor/a/x.go":         todo,
		"node_modules/m/i.js":   todo,
		"web/app.min.js":        todo,
		"legacy/old.go":         todo,
		"api/v1/x.pb.go":        todo,
		"mock.go":               {"// Code generated by mockgen. DO NOT EDIT.", "", "// TODO: x"},
		"gen.py":                {"# This file was automatically generated", "# TODO: x"},
		"schema.ts":             {"/* @generated */", "// TODO: x"},
		"img.go":                {"// TODO: x\x00binary"},
		"internal/real/keep.go": todo,
	})
	if got := marks(fs); len(fs) != 1 || fs[0].Path != "internal/real/keep.go" {
		t.Fatalf("got %v %+v", got, fs)
	}
}

func TestTodoFingerprintStableAcrossLineMoves(t *testing.T) {
	a, _ := todoNew(t, defTodo(), map[string][]string{"a.go": {"package a", "// TODO:   rework   cache"}})
	b, _ := todoNew(t, defTodo(), map[string][]string{"a.go": {"package a", "", "import \"os\"", "// TODO: rework cache"}})
	if len(a) != 1 || len(b) != 1 || a[0].Line == b[0].Line {
		t.Fatalf("%v %v", marks(a), marks(b))
	}
	if a[0].ID() != b[0].ID() {
		t.Error("moving the comment (or reflowing its spaces) must keep the id")
	}
	c, _ := todoNew(t, defTodo(), map[string][]string{"a.go": {"// TODO: rework the cache"}})
	d, _ := todoNew(t, defTodo(), map[string][]string{"b.go": {"// TODO: rework cache"}})
	if c[0].ID() == a[0].ID() || d[0].ID() == a[0].ID() {
		t.Error("another comment or another file is another thread")
	}
	// Adding the key removes the result (the thread is closed as fixed).
	e, st := todoNew(t, defTodo(), map[string][]string{"a.go": {"package a", "// TODO(PROJ-1): rework cache"}})
	if len(e) != 0 || st.State != StateRan {
		t.Fatalf("fixed: %v %+v", marks(e), st)
	}
}

func TestTodoSeverityBlockingAndKeywords(t *testing.T) {
	cfg := defTodo()
	cfg.Keywords = []string{"TODO", "NOCOMMIT"}
	cfg.Severity = core.SeverityMajor
	cfg.Blocking = true
	fs, _ := todoNew(t, cfg, map[string][]string{"a.go": {"// FIXME: x", "// NOCOMMIT: y", "// TODO: z"}})
	if got := marks(fs); !reflect.DeepEqual(got, []string{"2:NOCOMMIT", "3:TODO"}) {
		t.Fatalf("got %v", got)
	}
	for _, f := range fs {
		if f.Severity != core.SeverityMajor || !f.MayBlock || TodoTitle(f) != f.Rule+" без задачи" {
			t.Errorf("%+v", f)
		}
	}
	// require_task off and no key checker: nothing to do.
	cfg.RequireTask = false
	if _, st := todoNew(t, cfg, map[string][]string{"a.go": {"// TODO: z"}}); st.State != StateNotApplicable {
		t.Errorf("status %+v", st)
	}
}

func TestTodoCap(t *testing.T) {
	var lines []string
	for i := 0; i < todoMaxFindings+5; i++ {
		lines = append(lines, fmt.Sprintf("// TODO: item %d", i))
	}
	fs, st := todoNew(t, defTodo(), map[string][]string{"a.go": lines})
	if len(fs) != todoMaxFindings || st.State != StatePartial || st.Found != todoMaxFindings {
		t.Fatalf("%d %+v", len(fs), st)
	}
}

func TestTodoVerifyKeys(t *testing.T) {
	calls := map[string]int{}
	check := func(_ context.Context, key string) (KeyState, error) {
		calls[key]++
		switch key {
		case "PROJ-1", "PROJ-2":
			return KeyExists, nil
		case "PROJ-500":
			return KeyUnchecked, errors.New("jira: 502")
		}
		return KeyMissing, nil
	}
	cfg := defTodo()
	cfg.ProjectKeys = []string{"PROJ"}
	cfg.CheckKey = check
	fs, st := todoNew(t, cfg, map[string][]string{"a.go": {
		"// TODO(PROJ-1): exists",
		"// TODO(PROJ-999): missing",
		"// FIXME PROJ-999 again",
		"// TODO(PROJ-998, PROJ-2): one of them exists",
		"// TODO(PROJ-997): https://gitlab.example.com/g/p/-/issues/3",
		"// TODO: no key at all",
	}})
	if got := marks(fs); !reflect.DeepEqual(got, []string{"2:task-not-found", "3:task-not-found", "6:TODO"}) {
		t.Fatalf("got %v", got)
	}
	nf := fs[0]
	if nf.Todo.Key != "PROJ-999" || TodoTitle(nf) != "задача PROJ-999 не найдена" || nf.MayBlock ||
		nf.Severity != core.SeverityMinor || !strings.HasPrefix(nf.Message, "задача PROJ-999 не найдена — «TODO(PROJ-999)") {
		t.Errorf("not found: %+v %+v", nf, nf.Todo)
	}
	if calls["PROJ-999"] != 1 || calls["PROJ-997"] != 0 || calls["PROJ-998"] != 1 || calls["PROJ-2"] != 1 {
		t.Errorf("lookups: %v (each key once; keys next to another reference are not looked up)", calls)
	}
	if st.State != StateRan {
		t.Errorf("status %+v", st)
	}
	// A Jira error: the key counts as fine, the run is partial.
	fs, st = todoNew(t, cfg, map[string][]string{"a.go": {"// TODO(PROJ-500): jira is down"}})
	if len(fs) != 0 || st.State != StatePartial || !strings.Contains(st.Reason, "Jira") {
		t.Fatalf("error: %v %+v", marks(fs), st)
	}
	// require_task off: markers without a key are fine, keys are still checked.
	cfg.RequireTask = false
	fs, _ = todoNew(t, cfg, map[string][]string{"a.go": {"// TODO: nothing", "// TODO(PROJ-404)"}})
	if got := marks(fs); !reflect.DeepEqual(got, []string{"2:task-not-found"}) {
		t.Fatalf("require_task off: %v", got)
	}
}

func TestTodoSyntaxFor(t *testing.T) {
	for p, want := range map[string]*commentSyntax{
		"Makefile": synHash, "build/Dockerfile.prod": synHash, "BUILD.bazel": synPython, "Jenkinsfile": synC,
		"x.GO": synGoJS, "a/b.html.twig": synTwig, "a.min.json": nil, "LICENSE": synGeneric, ".env.local": synHash,
	} {
		if got := syntaxFor(p); got != want {
			t.Errorf("%s: wrong syntax", p)
		}
	}
}

func TestTodoMarkerBoundaries(t *testing.T) {
	kws := []string{"TODO", "FIXME"}
	for _, tc := range []struct {
		comment string
		kw      string
	}{
		{" TODO: x", "TODO"},
		{" @TODO x", "TODO"},
		{" see FIXME and TODO", "FIXME"},
		{" TODOs", ""},
		{" MYTODO", ""},
		{" TODO_LIST", ""},
		{" use context.TODO() here", ""},
		{" TODO() is a stub", ""},
		{" [TODO] x", "TODO"},
	} {
		kw, _, ok := findMarker([]string{tc.comment}, kws)
		if kw != tc.kw || ok != (tc.kw != "") {
			t.Errorf("%q: %q %v", tc.comment, kw, ok)
		}
	}
}
