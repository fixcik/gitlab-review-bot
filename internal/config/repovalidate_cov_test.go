package config

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/token"
)

// Keys written as an explicit key ("? enabled"), with a tag ("!!str
// enabled") or an anchor ("&a enabled") are the same key for the decoder,
// so the strict check accepts them too (it used to call them unknown).
func TestValidateRepoKeySpellings(t *testing.T) {
	for _, y := range []string{
		"? enabled\n: false\n",
		"!!str enabled: false\n",
		"&k enabled: false\n",
		"? !!str enabled\n: false\n",
	} {
		r, issues := ValidateRepo([]byte(y))
		if len(issues) > 0 || r == nil || r.Enabled == nil || *r.Enabled {
			t.Errorf("%q: r=%+v issues=%v", y, r, issues)
		}
	}
	// Nested sections are checked through such keys too.
	_, issues := ValidateRepo([]byte("? lint\n: {disabel: [x]}\n"))
	if len(issues) != 1 || issues[0].String() != `line 2: lint.disabel: unknown key (did you mean "lint.disable"?)` {
		t.Errorf("nested via explicit key: %v", issues)
	}
	// Other scalar keys are named as written; a quoted "<<" or "? <<" is not
	// a merge key (the decoder ignores it), so it is an unknown key.
	for y, want := range map[string]string{
		"1: x\n":                    `line 1: 1: unknown key (did you mean "ci"?)`,
		"true: x\n":                 `line 1: true: unknown key`,
		"\"<<\": {max_files: 3}\n":  `line 1: <<: unknown key (did you mean "ci"?)`,
		"? <<\n: {max_files: 3}\n":  `line 1: <<: unknown key (did you mean "ci"?)`,
		"lint:\n  2: x\n":           `line 2: lint.2: unknown key (did you mean "lint.ci"?)`,
		"--- &a\nlanguag: en\n":     `line 2: languag: unknown key (did you mean "language"?)`,
		"lint: &l {disabel: [x]}\n": `line 1: lint.disabel: unknown key (did you mean "lint.disable"?)`,
	} {
		_, issues := ValidateRepo([]byte(y))
		if len(issues) != 1 || issues[0].String() != want {
			t.Errorf("%q: %v, want %q", y, issues, want)
		}
	}
}

// Merge keys ("<<: *anchor") are skipped by the key check (their keys are
// checked where the anchor is defined) and applied by the decoder; a value
// that comes in only through a merge has no line of its own.
func TestValidateRepoMergeKeys(t *testing.T) {
	r, issues := ValidateRepo([]byte("<<: {max_files: 3}\n"))
	if len(issues) > 0 || r == nil || r.MaxFiles != 3 {
		t.Fatalf("top-level merge: r=%+v issues=%v", r, issues)
	}
	y := "path_instructions:\n" + // 1
		"  - &base {path: \"a/**\", instructions: x}\n" + // 2
		"  - <<: *base\n" + // 3
		"    instructionz: y\n" // 4
	_, issues = ValidateRepo([]byte(y))
	if len(issues) != 1 || issues[0].String() != `line 4: path_instructions[1].instructionz: unknown key (did you mean "path_instructions[1].instructions"?)` {
		t.Fatalf("merge in a list item: %v", issues)
	}
	r, issues = ValidateRepo([]byte("path_instructions:\n  - &base {path: \"a/**\", instructions: x}\n  - <<: *base\n"))
	if len(issues) > 0 || len(r.PathInstructions) != 2 || r.PathInstructions[1] != r.PathInstructions[0] {
		t.Fatalf("merged item: r=%+v issues=%v", r, issues)
	}
	// A section that is only in a merge is reported without a line, after
	// the issues that have one.
	_, issues = ValidateRepo([]byte("<<: {tbd: {max_lines: -1}}\nlanguag: en\n"))
	want := []string{`line 2: languag: unknown key (did you mean "language"?)`, `tbd.max_lines must be >= 0`}
	if len(issues) != 2 || issues[0].String() != want[0] || issues[1].String() != want[1] || issues[1].Line != 0 {
		t.Fatalf("merged section: %v", issues)
	}
}

// When the whole document fails to decode, each top-level key is decoded on
// its own: an empty (null) section is no problem, only the wrong type is.
func TestValidateRepoNullSectionNextToTypeError(t *testing.T) {
	_, issues := ValidateRepo([]byte("lint:\nmemory: ~\nmax_files: abc\n"))
	if len(issues) != 1 || !strings.HasPrefix(issues[0].String(), "line 3: max_files: cannot unmarshal") {
		t.Fatalf("issues: %v", issues)
	}
}

// A second document is reported on its own first line, with or without
// a "---" header.
func TestValidateRepoSecondDocumentLine(t *testing.T) {
	for y, want := range map[string]string{
		"enabled: true\n...\nx: 1\n":      "line 3: only one YAML document is allowed",
		"enabled: true\n...\n---\nx: 1\n": "line 3: only one YAML document is allowed",
		"enabled: true\n---\n":            "line 2: only one YAML document is allowed",
	} {
		_, issues := ValidateRepo([]byte(y))
		if len(issues) != 1 || issues[0].String() != want {
			t.Errorf("%q: %v, want %q", y, issues, want)
		}
	}
}

// Tags and anchors around the document body are transparent.
func TestValidateRepoTaggedDocument(t *testing.T) {
	for _, y := range []string{"--- !!map\nenabled: false\n", "--- &doc\nenabled: false\n"} {
		r, issues := ValidateRepo([]byte(y))
		if len(issues) > 0 || r == nil || r.Enabled == nil || *r.Enabled {
			t.Errorf("%q: r=%+v issues=%v", y, r, issues)
		}
	}
}

// RepoErrors match ErrRemovedKey only (and only with a removed key).
func TestRepoErrorsIs(t *testing.T) {
	_, err := ParseRepo([]byte("setup: [x]\n"))
	if !errors.Is(err, ErrRemovedKey) {
		t.Fatalf("removed key: %v", err)
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errors.New(ErrRemovedKey.Error())) {
		t.Fatal("RepoErrors must match no other error")
	}
	_, err = ParseRepo([]byte("languag: en\n"))
	if err == nil || errors.Is(err, ErrRemovedKey) {
		t.Fatalf("unknown key: %v", err)
	}
}

// yamlFields maps the yaml names of exported, tagged fields only.
func TestYAMLFields(t *testing.T) {
	type sample struct {
		g int `yaml:"g"`
		A int `yaml:"a"`
		B int `yaml:"b,omitempty"`
		C int
		D int `yaml:"-"`
		F int `yaml:",omitempty"`
		H int `yaml:"h"`
	}
	_ = sample{g: 1}
	got := yamlFields(reflect.TypeOf(sample{}))
	if want := map[string]int{"a": 1, "b": 2, "h": 6}; !reflect.DeepEqual(got, want) {
		t.Fatalf("yamlFields = %v, want %v", got, want)
	}
}

// Positions the parser does not know are line 0 ("no line").
func TestNodeLineUnknownPositions(t *testing.T) {
	if nodeLine(nil) != 0 {
		t.Error("nil node")
	}
	if nodeLine(ast.Null(nil)) != 0 {
		t.Error("node without a token")
	}
	if nodeLine(ast.String(&token.Token{Value: "x"})) != 0 {
		t.Error("token without a position")
	}
	if nodeLine(ast.String(&token.Token{Value: "x", Position: &token.Position{Line: 7}})) != 7 {
		t.Error("token with a position")
	}
	if docLine(&ast.DocumentNode{BaseNode: &ast.BaseNode{}}) != 0 {
		t.Error("empty document without a header")
	}
	if n, merge := keyName(&ast.MappingKeyNode{BaseNode: &ast.BaseNode{}}); n != "" || merge {
		t.Errorf("explicit key without a value: %q %v", n, merge)
	}
}

// Section errors: a message that names its key path is kept as it is, the
// others get the section key; every section is normalized.
func TestValidateRepoSectionKeys(t *testing.T) {
	for y, want := range map[string]string{
		"jira:\n  project_keys: [\"bad key\"]\n": `line 1: jira.project_keys: invalid key "BAD KEY"`,
		"migrations: {dialect: oracle}\n":        `line 1: migrations.dialect must be postgres or mysql`,
	} {
		_, issues := ValidateRepo([]byte(y))
		if len(issues) != 1 || issues[0].String() != want {
			t.Errorf("%q: %v, want %q", y, issues, want)
		}
	}
	r, issues := ValidateRepo([]byte("jira: {project_keys: [\" pay \"]}\nmigrations: {dialect: \" MySQL \"}\n"))
	if len(issues) > 0 || strings.Join(r.Jira.ProjectKeys, ",") != "PAY" || r.Migrations.Dialect != "mysql" {
		t.Fatalf("normalized: %+v %+v %v", r.Jira, r.Migrations, issues)
	}
}

// The size limit is named in KiB.
func TestRepoTooLargeMessage(t *testing.T) {
	_, issues := ValidateRepo(make([]byte, MaxRepoConfigBytes+1))
	if len(issues) != 1 || issues[0].String() != "the file is larger than 256 KiB" {
		t.Fatalf("issues: %v", issues)
	}
	if r, issues := ValidateRepo([]byte("enabled: true\n" + strings.Repeat("#", MaxRepoConfigBytes-14))); r == nil || len(issues) > 0 {
		t.Fatalf("at the limit: %v", issues)
	}
}

// Of two known keys at the same distance from a typo, the suggestion is
// the alphabetically first one, every time.
func TestValidateRepoSuggestionTieBreak(t *testing.T) {
	for i := 0; i < 30; i++ {
		_, issues := ValidateRepo([]byte("cp: 1\n")) // "ci" and "mcp" are both one edit away
		if len(issues) != 1 || issues[0].String() != `line 1: cp: unknown key (did you mean "ci"?)` {
			t.Fatalf("issues: %v", issues)
		}
	}
}

// Removed lint keys are removed only under lint: at the top level the same
// names are plain unknown keys.
func TestRemovedLintKeysOnlyUnderLint(t *testing.T) {
	for _, k := range removedRepoLintKeys {
		_, err := ParseRepo([]byte(k + ": []\n"))
		if err == nil || errors.Is(err, ErrRemovedKey) || !strings.Contains(err.Error(), "line 1: "+k+": unknown key") {
			t.Errorf("%s: %v", k, err)
		}
	}
}
