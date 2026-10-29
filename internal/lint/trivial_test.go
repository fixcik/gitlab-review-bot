package lint

import "testing"

func TestCompareCode(t *testing.T) {
	for _, tc := range []struct {
		name, path, before, after string
		want                      CodeDiff
	}{
		{"identical", "a.go", "package a\n", "package a\n", CodeSame},
		{"go reindent", "a.go", "func f() {\nreturn  1\n}\n", "func f() {\n\treturn 1\n}\n", CodeWhitespace},
		{"go blank lines", "a.go", "package a\nvar x = 1\n", "package a\n\n\nvar x = 1\n", CodeWhitespace},
		{"go operator spacing", "a.go", "x := a+b\n", "x := a + b\n", CodeWhitespace},
		{"go call reflow", "a.go", "f(a, b)\n", "f(\n\ta,\n\tb,\n)\n", CodeChanged}, // trailing comma added: a token
		{"go call reflow no comma", "a.go", "f(a, b)\n", "f(\n\ta, b)\n", CodeWhitespace},
		{"go line comment", "a.go", "x := 1 // one\n", "x := 1 // the first\n", CodeComments},
		{"go block comment", "a.go", "/* a */\nx := 1\n", "/*\n * b\n */\nx := 1\n", CodeComments},
		{"go new comment line", "a.go", "x := 1\n", "// why\nx := 1\n", CodeComments},
		{"go code change", "a.go", "x := 1\n", "x := 2\n", CodeChanged},
		{"go deleted code", "a.go", "x := 1\ny := 2\n", "x := 1\n", CodeChanged},
		{"go word split", "a.go", "return x\n", "returnx\n", CodeChanged},
		{"go newline between words", "a.go", "return x\n", "return\nx\n", CodeChanged},
		{"go unary minus", "a.go", "y := a - -b\n", "y := a--b\n", CodeChanged},
		{"go string spaces", "a.go", "s := \"a b\"\n", "s := \"a  b\"\n", CodeChanged},
		{"go comment marker in string", "a.go", "s := \"// x\"\n", "s := \"// y\"\n", CodeChanged},
		{"go raw string", "a.go", "s := `a\nb`\n", "s := `a\n  b`\n", CodeChanged},
		{"js comment", "web/a.ts", "const a = 1; // x\n", "const a = 1;\n", CodeComments},
		{"python indent", "a.py", "if x:\n    y = 1\n", "if x:\n  y = 1\n", CodeChanged},
		{"python inner spaces", "a.py", "if x:\n    y=1\n", "if x:\n    y = 1  # set\n", CodeComments},
		{"python docstring", "a.py", "def f():\n    \"\"\"Old.\"\"\"\n    return 1\n", "def f():\n    \"\"\"New text.\"\"\"\n    return 1\n", CodeComments},
		{"python blank lines", "a.py", "a = 1\nb = 2\n", "a = 1\n\n\nb = 2\n", CodeWhitespace},
		{"yaml spaces", "ci.yml", "key: v\n", "key:  v\n", CodeWhitespace},
		{"yaml colon", "ci.yml", "key: v\n", "key:v\n", CodeChanged},
		{"yaml comment line", "ci.yml", "a: 1\n", "# note\na: 1\n", CodeComments},
		{"yaml trailing hash is text", "ci.yml", "run: echo x # a\n", "run: echo x # b\n", CodeChanged},
		{"yaml indent", "ci.yml", "a:\n  b: 1\n", "a:\n    b: 1\n", CodeChanged},
		{"shell spacing kept", "run.sh", "a=b\n", "a = b\n", CodeChanged},
		{"shell comment", "run.sh", "echo hi\n", "# greet\necho hi\n", CodeComments},
		{"unknown type", "data.weird", "a\n", "a \n", CodeChanged},
		{"json", "a.json", "{\"a\":1}\n", "{\"a\": 1}\n", CodeChanged},
		{"binary", "a.go", "a\x00", "b\x00", CodeChanged},
		{"crlf", "a.go", "x := 1\r\ny := 2\r\n", "x := 1\ny := 2\n", CodeWhitespace},
		{"preprocessor join", "a.c", "#define X 1\n#define Y 2\n", "#define X 1 #define Y 2\n", CodeChanged},
	} {
		if got := CompareCode(tc.path, []byte(tc.before), []byte(tc.after)); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The todo check must not change: the lexer additions only record code.
func TestScanLineSegments(t *testing.T) {
	var st lexState
	ls := scanLine(`x := "a // b" /* c */ + y // d`, synGoJS, &st)
	if len(ls.comments) != 2 || ls.comments[0] != " c " || ls.comments[1] != " d" || !ls.code {
		t.Fatalf("comments %q code %v", ls.comments, ls.code)
	}
	var got string
	for _, s := range ls.segs {
		switch {
		case s.str:
			got += "[" + s.text + "]"
		case s.cmt:
			got += "{" + s.open + "|" + s.text + "}"
		default:
			got += s.text
		}
	}
	if want := `x := ["a // b"] {/*| c } + y {//| d}`; got != want {
		t.Errorf("segs %q, want %q", got, want)
	}
}
