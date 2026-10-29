package lint

// Code equivalence for trivial incremental pushes (review.skip_trivial_increments,
// internal/review/trivial.go): two versions of a file are "the same code"
// when they differ only in comments and in whitespace that cannot change
// the program. It reuses the comment lexer of the todo check (todo.go), so
// the same limits apply (heredocs, raw strings of some languages, regex
// literals); every uncertainty must make the answer "different".
//
// Whitespace rules by file type (indentSensitive, looseInline):
//   - C-like languages (Go, JS/TS, Java, C#, C/C++, Rust, Kotlin, Swift,
//     PHP, CSS/SCSS, SQL, HCL, Lua, …): whitespace between tokens is
//     dropped unless both neighbours are word characters or both are
//     operator characters ("return x", "a - -b"); a line break between two
//     words stays a line break (automatic semicolons in Go and JS).
//   - Python and the layout languages (Haskell, Elm, F#, CoffeeScript,
//     Nim): line by line, the indentation is kept exactly, whitespace
//     inside a line as for C-like languages.
//   - everything else (YAML, shell, Dockerfile, Makefile, Ruby, HTML,
//     templates, config files): line by line, indentation kept, runs of
//     whitespace inside a line collapse to one space but never disappear.
//
// String literals are kept verbatim. Files without a known comment syntax
// (the generic fallback, JSON, plain text, binaries) are never equivalent
// unless they are byte-identical.
//
// Fail-closed: a comment counts as a comment only when the lexer is sure
// and nothing reads it.
//   - The lexer is not sure (lineScan.unsure, unsureText): an unterminated
//     quote (a multi-line string, a Java text block), a comment starter
//     right after a backslash or "$" (a regular expression), a Python
//     docstring followed by code, a heredoc, a Lua long string, SQL dollar
//     quoting, PHP's "?>", a Ruby/Perl data section, a raw string with "#",
//     a block comment or a string still open at the end. Such a file is
//     never trivial.
//   - Python triple-quoted strings are inert only as a statement of their
//     own (bracket depth 0, no backslash continuation, no f/b prefix,
//     nothing but a comment after them); otherwise they are strings.
//   - A "* " line outside a block comment is code (the docblock guess is
//     for the todo check's diff-only fallback).
//   - Comments that a compiler, interpreter or tool reads (sensitiveComment:
//     directives like //go:build, # type:, eslint-disable, "@Annotation",
//     "key: value", doctests, template tags) are compared verbatim as
//     code, together with the comments next to them (a docblock, a run of
//     line comments): any change to them is a code change.
//   - Every comment is code (allSensitive) in files mixing languages or
//     whose comments the lexer cannot tell from text: cgo files (the
//     preamble before import "C" is C code), HTML with <script>/<style>,
//     JSX, Vue/Svelte/Astro, MDX, JSP, TeX; "<!--" comments in Twig and
//     Handlebars templates (their tags run inside them).
//   - Dockerfile, .properties, .gitignore and similar files have whole-line
//     "#" comments only (synHashLine), YAML too, and the text of a YAML
//     block scalar ("run: |") is never a comment.

import (
	"path"
	"regexp"
	"strings"
)

// CodeDiff is the result of CompareCode.
type CodeDiff int

const (
	// CodeChanged: the code differs (or the file type is not understood).
	CodeChanged CodeDiff = iota
	// CodeSame: identical bytes.
	CodeSame
	// CodeWhitespace: only whitespace differs (formatting).
	CodeWhitespace
	// CodeComments: comments differ (and maybe whitespace), code is the same.
	CodeComments
)

// CompareCode compares two versions of the file at p.
func CompareCode(p string, before, after []byte) CodeDiff {
	if string(before) == string(after) {
		return CodeSame
	}
	if binary(before) || binary(after) {
		return CodeChanged
	}
	syn := trivialSyntax(p)
	if syn == nil {
		return CodeChanged
	}
	b, a := string(before), string(after)
	if re := unsureText(p, syn); re != nil && (re.MatchString(b) || re.MatchString(a)) {
		return CodeChanged
	}
	all := allSensitive(p, syn, b) || allSensitive(p, syn, a)
	indent, loose := whitespaceRules(p, syn)
	bc, bk, bok := normalizedCode(b, syn, indent, loose, all)
	ac, ak, aok := normalizedCode(a, syn, indent, loose, all)
	switch {
	case !bok || !aok || bc != ac:
		return CodeChanged
	case bk != ak:
		return CodeComments
	}
	return CodeWhitespace
}

func binary(b []byte) bool {
	head := b
	if len(head) > 8000 {
		head = head[:8000]
	}
	return strings.IndexByte(string(head), 0) >= 0
}

var (
	// synYAML: YAML comments only as whole lines ("#" after a value may be
	// part of a plain scalar), quotes only at the start of a scalar, block
	// scalars are text.
	synYAML = &commentSyntax{line: []string{"#"}, quotes: `"'`, lineStart: strSet("#"), scalarQuotes: true, yamlBlocks: true}
	// synHashLine: "#" comments only as whole lines (Dockerfile, config
	// files: a "#" after a value belongs to it).
	synHashLine = &commentSyntax{line: []string{"#"}, quotes: `"'`, lineStart: strSet("#")}
)

// inlineHash: the types of synHash where "#" after code starts a comment.
var inlineHash = strSet(".sh", ".bash", ".zsh", ".ksh", ".fish", ".r", ".pl", ".pm", ".cmake", ".mk", ".mak",
	".ps1", ".psm1", ".awk", ".nix", ".gd", ".toml", "makefile", "gnumakefile", "gemfile", "rakefile",
	"vagrantfile", "brewfile", "cmakelists.txt")

// trivialSyntax is the comment syntax for the comparison: the todo
// check's, except that unknown types (the generic guess) and types without
// comments are not compared at all.
func trivialSyntax(p string) *commentSyntax {
	lower := strings.ToLower(path.Base(p))
	switch path.Ext(lower) {
	case ".yaml", ".yml":
		return synYAML
	}
	syn := syntaxFor(p)
	switch {
	case syn == synGeneric:
		return nil
	case syn == synHash && !inlineHash[path.Ext(lower)] && !inlineHash[lower]:
		return synHashLine
	}
	return syn
}

var (
	heredocShell = `<<[-~]?\s*['"\\]?[A-Za-z_]`
	heredocOther = `<<<?[-~]?['"]?[A-Za-z_]`
	dataSection  = `(?m)^__(END|DATA)__\s*$`
	longBracket  = `\[=*\[`

	unsureBySyntax = map[*commentSyntax]*regexp.Regexp{
		synHash:     regexp.MustCompile(heredocShell + "|" + dataSection),
		synHashLine: regexp.MustCompile(heredocShell),
		synRuby:     regexp.MustCompile(heredocOther + "|" + dataSection),
		synPHP:      regexp.MustCompile(heredocOther + `|\?>`),
		synHCL:      regexp.MustCompile(heredocOther),
		synLua:      regexp.MustCompile(longBracket),
		synSQL:      regexp.MustCompile(`\$[A-Za-z_]*\$`), // dollar quoting
		synC:        regexp.MustCompile(`#"`),             // Swift, C# raw strings
		synRust:     regexp.MustCompile(`#"`),             // r#"…"#
	}
	unsureByType = map[string]*regexp.Regexp{
		".nix":           regexp.MustCompile(heredocShell + "|''"),
		".cmake":         regexp.MustCompile(heredocShell + "|" + longBracket),
		"cmakelists.txt": regexp.MustCompile(heredocShell + "|" + longBracket),
	}
)

// unsureText: text that the lexer does not understand (heredocs, long
// strings) makes a file of this type never trivial.
func unsureText(p string, syn *commentSyntax) *regexp.Regexp {
	lower := strings.ToLower(path.Base(p))
	if re, ok := unsureByType[lower]; ok {
		return re
	}
	if re, ok := unsureByType[path.Ext(lower)]; ok {
		return re
	}
	return unsureBySyntax[syn]
}

// cgoRe: a Go file that imports "C" (its preamble comment is C code).
var cgoRe = regexp.MustCompile(`(?m)^\s*(import\s+)?"C"\s*(//.*)?$`)

// allSensitive: every comment of the file is compared as code.
func allSensitive(p string, syn *commentSyntax, text string) bool {
	lower := strings.ToLower(path.Base(p))
	switch syn {
	case synVue, synMD, synTeX:
		return true
	case synHTML, synTwig, synHBS:
		t := strings.ToLower(text)
		return path.Ext(lower) == ".jsp" || strings.Contains(t, "<script") || strings.Contains(t, "<style")
	case synGoJS:
		if path.Ext(lower) == ".go" {
			return cgoRe.MatchString(text)
		}
		return strings.Contains(text, "</") || strings.Contains(text, "/>") // JSX
	}
	return false
}

var (
	// directiveRe: words of compiler, interpreter and tool directives
	// anywhere in a comment.
	directiveRe = regexp.MustCompile(`(?i)` + strings.Join([]string{
		`\bgo:\w`, `\+build\b`, `\bcgo\b`, `lint\b`, `\bnoqa\b`, `\bnosec\b`, `\bnosonar\b`, `\bnoinspection\b`,
		`\bpragma\b`, `\bpyright\b`, `\bmypy\b`, `\bpyre-`, `\bpytype\b`, `\btype\s*:`, `\bfmt\s*:`, `\bisort\s*:`,
		`\bcoding[:=]`, `-\*-`, `prettier-ignore`, `\bistanbul\b`, `\bc8\s`, `webpack`, `vite-ignore`,
		`sourcemappingurl`, `sourceurl`, `__pure__`, `__inline__`, `__no_side_effects__`, `\bshellcheck\b`,
		`\brubocop\b`, `frozen_string_literal`, `\bencoding\s*:`, `warn_indent`, `shareable_constant_value`,
		`\bresharper\b`, `swift-tools-version`, `\bcheckov\b`, `\btfsec\b`, `\btrivy\b`, `\bkics\b`, `semgrep\b`,
		`\bcodeql\b`, `\bsnyk\b`, `\blgtm\b`, `do not edit`, `\bts-`, `\bsyntax\s*=`, `\bescape\s*=`, `\bcheck\s*=`,
		`^\s*(globals?|exported)\s`, `\b(disable|enable)[-=:]`, `-(ignore|disable|enable)\b`, `\bignore[-_:=]`,
		`\bphpstan\b`, `\bpsalm\b`, `\bphpcs\b`, `\bkubebuilder\b`, `\bswagger:`, `\bgoose\b`, `\bmigrate:`,
		`\bliquibase\b`, `\bchangeset\b`, `>>>`, "```", `~~~`, `\{\{`, `\{%`, `<%`, `\$\{`, `#\{`,
		`\\u+000[ad]`, `\?\?/`,
	}, "|"))
	// annotationRe: "@Name" (Java and PHP annotations, JSDoc tags, @ts-…).
	annotationRe = regexp.MustCompile(`(^|[^\w@.])@[A-Za-z_]`)
	// keyValueRe: "key: value" or "key=value" at the start of a comment, the
	// shape of most directives (# type:, -- name:, # frozen_string_literal:).
	keyValueRe = regexp.MustCompile(`^([\w.\-/]+)(\s+[\w.\-/]+)?\s*[:=]`)
	// proseLabels may start a "label: text" comment.
	proseLabels = strSet("todo", "fixme", "note", "notes", "nb", "xxx", "hack", "bug", "warning", "warn", "safety",
		"see", "example", "examples", "returns", "return", "args", "arguments", "param", "params", "parameters",
		"raises", "raise", "yields", "yield", "attributes", "usage", "deprecated", "important", "why", "summary",
		"keyword", "other")
)

// sensitiveComment: the comment piece may be read by a compiler, an
// interpreter or a tool.
func sensitiveComment(syn *commentSyntax, c codeSeg) bool {
	t := c.text
	if c.open != "" && c.open[0] != '"' && c.open[0] != '\'' && t != "" && !banner(c.open, t[0]) {
		return true // "//go:build", "#!", "/*!", "--1", "<!--#include": no space after the opener
	}
	tt := strings.TrimLeft(strings.TrimLeft(strings.TrimLeft(t, " \t"), "*"), " \t")
	if tt != "" && strings.IndexByte("!+@$<[/", tt[0]) >= 0 {
		return true
	}
	if m := keyValueRe.FindStringSubmatch(tt); m != nil && !proseLabels[strings.ToLower(m[1])] {
		return true
	}
	return annotationRe.MatchString(t) || directiveRe.MatchString(t) ||
		strings.HasSuffix(strings.TrimRight(t, " \t"), "\\") || // a line splice (C)
		((syn == synTwig || syn == synHBS) && c.open == "<!--") // template tags run inside
}

// banner: what may follow a comment opener directly in prose ("// x",
// "/** x", "## x", "---").
func banner(open string, c byte) bool {
	switch {
	case c == ' ' || c == '\t':
		return true
	case open == "--": // "--x" is an operator (Haskell) or "- -x" (MySQL)
		return c == '-'
	case c == open[len(open)-1] && open != "//": // "///" is a doc comment
		return true
	}
	return strings.IndexByte("-=*~_", c) >= 0
}

// markSensitive turns sensitive comments into code: every piece of a
// group (pieces with no code and no blank line between them: a block
// comment, a docblock, a run of line comments) that has a sensitive piece,
// or every piece when all is set.
func markSensitive(sc []lineScan, syn *commentSyntax, all bool) {
	var group []*codeSeg
	sensitive := false
	end := func() {
		for _, c := range group {
			if sensitive {
				*c = codeSeg{text: "\x00" + c.open + "\x01" + c.text + "\x00", str: true}
			}
		}
		group, sensitive = group[:0], false
	}
	for i := 1; i < len(sc); i++ {
		blank := true
		for k := range sc[i].segs {
			c := &sc[i].segs[k]
			switch {
			case c.cmt:
				group = append(group, c)
				sensitive = sensitive || all || sensitiveComment(syn, *c)
				blank = false
			case c.str || strings.TrimSpace(c.text) != "":
				end()
				blank = false
			}
		}
		if blank {
			end()
		}
	}
	end()
}

// whitespaceRules: indent = line breaks and indentation are significant;
// loose = whitespace between tokens may be dropped (see the file comment).
func whitespaceRules(p string, syn *commentSyntax) (indent, loose bool) {
	lower := strings.ToLower(path.Base(p))
	switch path.Ext(lower) {
	case ".py", ".pyi", ".pyw", ".bzl", ".star", ".hs", ".lhs", ".elm", ".purs", ".fs", ".fsx", ".coffee", ".nim":
		return true, true
	case ".sass", ".styl":
		return true, false
	}
	switch lower {
	case "build", "build.bazel", "workspace":
		return true, true
	}
	switch syn {
	case synC, synRust, synGoJS, synCSS, synSCSS, synPHP, synHCL, synSQL, synLua:
		return false, true
	}
	return true, false
}

// normalizedCode returns the file's code and its comments, normalized;
// ok=false: the lexer is not sure about the file.
func normalizedCode(text string, syn *commentSyntax, indent, loose, all bool) (code, comments string, ok bool) {
	lines := splitLines(strings.ReplaceAll(text, "\r\n", "\n"))
	sc, st := scanFile(lines, syn)
	if st.block > 0 || st.str != "" || (syn.triple && st.depth != 0) {
		return "", "", false
	}
	for _, ls := range sc {
		if ls.unsure {
			return "", "", false
		}
	}
	markSensitive(sc, syn, all)
	var cm strings.Builder
	for i := 1; i < len(sc); i++ {
		for _, c := range sc[i].comments {
			if f := strings.Fields(c); len(f) > 0 {
				cm.WriteString(strings.Join(f, " "))
				cm.WriteByte('\n')
			}
		}
	}
	if indent {
		return perLineCode(lines, sc, loose), cm.String(), true
	}
	w := normWriter{loose: loose}
	for i := 1; i < len(sc); i++ {
		ls := sc[i]
		if ls.contStr {
			w.pend = 0
			w.str("\n") // the line break is inside the string
		}
		w.segs(ls.segs)
		w.pend = max(w.pend, 2)
	}
	return w.b.String(), cm.String(), true
}

// perLineCode: one output line per line with code, indentation kept.
func perLineCode(lines []srcLine, sc []lineScan, loose bool) string {
	var b strings.Builder
	for i := 1; i < len(sc); i++ {
		ls := sc[i]
		if !ls.contStr && !hasCode(ls) {
			continue // blank or comment only
		}
		if !ls.contStr {
			t := lines[i].text
			b.WriteString(t[:len(t)-len(strings.TrimLeft(t, " \t"))])
		}
		w := normWriter{loose: loose}
		w.segs(ls.segs)
		b.WriteString(w.b.String())
		b.WriteByte('\n')
	}
	return b.String()
}

func hasCode(ls lineScan) bool {
	for _, s := range ls.segs {
		if s.str || (!s.cmt && strings.TrimSpace(s.text) != "") {
			return true
		}
	}
	return false
}

// normWriter writes code with normalized whitespace: pend is the pending
// whitespace (0 none, 1 space, 2 a line break).
type normWriter struct {
	b     strings.Builder
	loose bool
	pend  int
	last  byte
}

// segs writes the pieces of one line: code normalized, strings verbatim.
func (w *normWriter) segs(segs []codeSeg) {
	for _, s := range segs {
		switch {
		case s.str:
			w.str(s.text)
		case s.cmt: // a comment separates tokens like a space
			w.pend = max(w.pend, 1)
		default:
			w.code(s.text)
		}
	}
}

func (w *normWriter) code(s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\t', '\f', '\v', '\r':
			w.pend = max(w.pend, 1)
		default:
			w.flush(c)
			w.b.WriteByte(c)
			w.last = c
		}
	}
}

func (w *normWriter) str(s string) {
	if s == "" {
		return
	}
	w.flush(s[0])
	w.b.WriteString(s)
	w.last = s[len(s)-1]
}

// flush writes the pending whitespace before next when it matters.
func (w *normWriter) flush(next byte) {
	// A line break before "#" stays (C preprocessor lines).
	if w.pend > 0 && w.b.Len() > 0 && (!w.loose || keepSpace(w.last, next) || (w.pend == 2 && next == '#')) {
		if w.pend == 2 && w.loose {
			w.b.WriteByte('\n')
		} else {
			w.b.WriteByte(' ')
		}
	}
	w.pend = 0
}

func keepSpace(a, b byte) bool {
	return (wordByte(a) && wordByte(b)) || (opByte(a) && opByte(b))
}

func wordByte(c byte) bool { return isWordByte(c) || c == '$' || c >= 0x80 }

func opByte(c byte) bool { return strings.IndexByte("+-*/%&|^!~<>=.:?@#\\", c) >= 0 }
