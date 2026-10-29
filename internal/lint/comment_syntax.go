package lint

import (
	"path"
	"strings"
)

// commentSyntax is the comment and string syntax of a file type.
type commentSyntax struct {
	line  []string    // line comment starters
	block [][2]string // block comment delimiters
	// quotes delimit one-line strings; multi are quotes whose strings may
	// span lines (Go raw strings, JS template literals).
	quotes, multi string
	// triple: Python triple-quoted strings; one that starts a line is a
	// docstring and counts as a comment.
	triple bool
	// spaced starters count only at the start of a line or after
	// whitespace ("#" in shell/YAML, "//" in SCSS); lineStart ones only
	// at the start of a line ("//" in HTML-like templates, ";" in INI).
	spaced, lineStart map[string]bool
	// star: a line starting with "* " is a docblock line (/** … */) when
	// the lines before are not known (the diff-only fallback).
	star bool
	// primes: 'a not closed right after the name is a lifetime or a type
	// variable (Rust, F#), not a quote.
	primes bool
	// scalarQuotes: a quote opens a string only at the start of a YAML
	// scalar; yamlBlocks: block scalars ("key: |") are text, not comments.
	// Used by CompareCode only (synYAML).
	scalarQuotes, yamlBlocks bool
}

// notQuote: the quote at s[i] opens no string (see primes, scalarQuotes).
func (syn *commentSyntax) notQuote(s string, i int) bool {
	switch {
	case syn.primes && s[i] == '\'':
		j := i + 1
		for j < len(s) && wordByte(s[j]) {
			j++
		}
		return j > i+1 && (j == len(s) || s[j] != '\'')
	case syn.scalarQuotes:
		k := strings.TrimRight(s[:i], " \t")
		return k != "" && strings.IndexByte(":-[{,?", k[len(k)-1]) < 0
	}
	return false
}

func strSet(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

var (
	cBlock    = [2]string{"/*", "*/"}
	htmlBlock = [2]string{"<!--", "-->"}

	synC       = &commentSyntax{line: []string{"//"}, block: [][2]string{cBlock}, quotes: `"'`, star: true}
	synRust    = &commentSyntax{line: []string{"//"}, block: [][2]string{cBlock}, quotes: `"'`, star: true, primes: true}
	synGoJS    = &commentSyntax{line: []string{"//"}, block: [][2]string{cBlock}, quotes: "\"'`", multi: "`", star: true}
	synCSS     = &commentSyntax{block: [][2]string{cBlock}, quotes: `"'`, star: true}
	synSCSS    = &commentSyntax{line: []string{"//"}, block: [][2]string{cBlock}, quotes: `"'`, spaced: strSet("//"), star: true}
	synPHP     = &commentSyntax{line: []string{"//", "#"}, block: [][2]string{cBlock}, quotes: "\"'`", star: true}
	synHash    = &commentSyntax{line: []string{"#"}, quotes: `"'`, spaced: strSet("#")}
	synPython  = &commentSyntax{line: []string{"#"}, quotes: `"'`, triple: true}
	synRuby    = &commentSyntax{line: []string{"#"}, quotes: `"'`}
	synHCL     = &commentSyntax{line: []string{"#", "//"}, block: [][2]string{cBlock}, quotes: `"`, star: true}
	synSQL     = &commentSyntax{line: []string{"--"}, block: [][2]string{cBlock}, quotes: "\"'`", star: true}
	synDash    = &commentSyntax{line: []string{"--"}, block: [][2]string{{"{-", "-}"}}, quotes: `"`}
	synLua     = &commentSyntax{line: []string{"--"}, quotes: `"'`}
	synHTML    = &commentSyntax{block: [][2]string{htmlBlock}, quotes: `"`}
	synMD      = &commentSyntax{block: [][2]string{htmlBlock}}
	synVue     = &commentSyntax{line: []string{"//"}, block: [][2]string{htmlBlock, cBlock}, quotes: "\"'`", multi: "`", spaced: strSet("//")}
	synTwig    = &commentSyntax{block: [][2]string{{"{#", "#}"}, htmlBlock}, quotes: `"`}
	synHBS     = &commentSyntax{block: [][2]string{{"{{!--", "--}}"}, {"{{!", "}}"}, htmlBlock}, quotes: `"`}
	synINI     = &commentSyntax{line: []string{";", "#"}, quotes: `"`, lineStart: strSet(";", "#")}
	synLisp    = &commentSyntax{line: []string{";"}, quotes: `"`}
	synPercent = &commentSyntax{line: []string{"%"}, quotes: `"`}
	synTeX     = &commentSyntax{line: []string{"%"}}
	synGeneric = &commentSyntax{line: []string{"//", "#", "--"}, block: [][2]string{cBlock, htmlBlock}, quotes: `"'`, spaced: strSet("//", "#", "--"), star: true}
)

var synByExt = map[string]*commentSyntax{}

func init() {
	add := func(s *commentSyntax, exts ...string) {
		for _, e := range exts {
			synByExt[e] = s
		}
	}
	add(synGoJS, ".go", ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts")
	add(synC, ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx", ".m", ".mm", ".java", ".kt", ".kts",
		".scala", ".sc", ".groovy", ".gradle", ".cs", ".swift", ".dart", ".proto", ".jsonc", ".json5",
		".sol", ".zig", ".v", ".graphql", ".gql", ".prisma", ".thrift", ".jenkinsfile")
	add(synRust, ".rs", ".fs", ".fsx")
	add(synCSS, ".css")
	add(synSCSS, ".scss", ".less", ".sass", ".styl")
	add(synPHP, ".php", ".phtml", ".inc")
	add(synPython, ".py", ".pyi", ".pyw", ".bzl", ".star")
	add(synRuby, ".rb", ".rake", ".gemspec", ".cr", ".ex", ".exs", ".jl", ".nim", ".coffee")
	add(synHash, ".sh", ".bash", ".zsh", ".ksh", ".fish", ".yaml", ".yml", ".toml", ".r", ".pl", ".pm",
		".cmake", ".mk", ".mak", ".dockerfile", ".conf", ".cfg", ".properties", ".env", ".gitignore",
		".dockerignore", ".editorconfig", ".ps1", ".psm1", ".tcl", ".awk", ".nix", ".service", ".gd")
	add(synHCL, ".tf", ".tfvars", ".hcl")
	add(synSQL, ".sql", ".psql", ".pgsql", ".plsql", ".hql", ".ddl")
	add(synDash, ".hs", ".lhs", ".elm", ".purs", ".ada", ".adb", ".ads", ".vhd", ".vhdl")
	add(synLua, ".lua")
	add(synHTML, ".html", ".htm", ".xhtml", ".xml", ".xsd", ".xsl", ".xslt", ".svg", ".plist", ".csproj", ".vbproj", ".props", ".targets", ".jsp")
	add(synMD, ".md", ".markdown", ".mdx", ".rst")
	add(synVue, ".vue", ".svelte", ".astro")
	add(synTwig, ".twig", ".j2", ".jinja", ".jinja2", ".njk")
	add(synHBS, ".hbs", ".handlebars", ".mustache")
	add(synINI, ".ini")
	add(synLisp, ".clj", ".cljs", ".cljc", ".edn", ".el", ".lisp", ".lsp", ".scm", ".ss", ".rkt", ".asm")
	add(synPercent, ".erl", ".hrl")
	add(synTeX, ".tex", ".sty", ".cls", ".bib")
	// No comments at all: never flagged.
	for _, e := range []string{".json", ".lock", ".sum", ".txt", ".csv", ".tsv", ".log", ".map", ".snap",
		".png", ".jpg", ".jpeg", ".gif", ".ico", ".webp", ".pdf", ".zip", ".gz", ".jar", ".woff", ".woff2", ".ttf", ".eot"} {
		synByExt[e] = nil
	}
}

// syntaxFor picks the comment syntax by file name (nil: no comments).
func syntaxFor(p string) *commentSyntax {
	base := path.Base(p)
	lower := strings.ToLower(base)
	switch {
	case lower == "build" || lower == "build.bazel" || lower == "workspace":
		return synPython
	case lower == "dockerfile" || lower == "containerfile" || strings.HasPrefix(lower, "dockerfile."),
		lower == "makefile" || lower == "gnumakefile", lower == "cmakelists.txt",
		lower == "gemfile" || lower == "rakefile" || lower == "vagrantfile" || lower == "brewfile",
		lower == "procfile", lower == ".gitignore", lower == ".dockerignore", lower == ".gitattributes",
		strings.HasPrefix(lower, ".env"), lower == "codeowners":
		return synHash
	case lower == "jenkinsfile":
		return synC
	case strings.HasSuffix(lower, ".html.twig"):
		return synTwig
	}
	if s, ok := synByExt[path.Ext(lower)]; ok {
		return s
	}
	return synGeneric
}
