package config

// The JSON Schema of .reviewbot.yaml (docs/reviewbot.schema.json) is
// generated from the Repo structs and their doc comments. Editors with the
// YAML language server use it through the first line of .reviewbot.yaml:
//
//	# yaml-language-server: $schema=<URL of docs/reviewbot.schema.json>
//
// Regenerate after changing the repo config:
//
//	UPDATE_SCHEMA=1 go test ./internal/config -run TestRepoSchemaUpToDate

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const repoSchemaPath = "../../docs/reviewbot.schema.json"

// schemaEnums are the closed value sets, by key path ("[]" = list items).
func schemaEnums() map[string][]string {
	sev := []string{"critical", "major", "minor", "nit"}
	return map[string][]string{
		"lint.disable[]":           LintTools,
		"lint.ci[].format":         ciFormats,
		"lint.ci[].report":         {CIReportCodeQuality},
		"lint.ci[].severity":       sev,
		"todo.severity":            sev,
		"migrations.dialect":       {"postgres", "mysql"},
		"labels.rules[].when_fact": LabelFacts(),
	}
}

// fieldDocs reads the doc comments of the struct fields of this package:
// "Type.Field" -> text.
func fieldDocs(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fl := range st.Fields.List {
				text := ""
				if fl.Doc != nil {
					text = fl.Doc.Text()
				} else if fl.Comment != nil {
					text = fl.Comment.Text()
				}
				text = strings.Join(strings.Fields(text), " ")
				for _, id := range fl.Names {
					if text != "" {
						docs[ts.Name.Name+"."+id.Name] = text
					}
				}
			}
			return true
		})
	}
	return docs
}

type schemaGen struct {
	docs  map[string]string
	enums map[string][]string
}

func (g *schemaGen) typ(t reflect.Type, path string) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case reflect.TypeOf(Duration(0)):
		return map[string]any{"type": "string", "pattern": `^(0|([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+)$`,
			"description": "Go duration: 90s, 10m, 1h30m."}
	case reflect.TypeOf(FactList(nil)):
		item := g.enum(map[string]any{"type": "string"}, path)
		return map[string]any{"anyOf": []any{item, map[string]any{"type": "array", "items": item}}}
	}
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if !f.IsExported() || name == "" || name == "-" {
				continue
			}
			p := joinKey(path, name)
			s := g.typ(f.Type, p)
			if d := g.docs[t.Name()+"."+f.Name]; d != "" {
				if old, ok := s["description"].(string); ok {
					d += " " + old
				}
				s["description"] = d
			}
			props[name] = s
		}
		return map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": g.typ(t.Elem(), path+"[]")}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.typ(t.Elem(), path+".*")}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.String:
		return g.enum(map[string]any{"type": "string"}, path)
	case reflect.Int, reflect.Int64, reflect.Int32:
		return map[string]any{"type": "integer"}
	case reflect.Float64, reflect.Float32:
		return map[string]any{"type": "number"}
	}
	return map[string]any{}
}

func (g *schemaGen) enum(s map[string]any, path string) map[string]any {
	if e, ok := g.enums[path]; ok {
		s["enum"] = e
	}
	return s
}

func generateRepoSchema(t *testing.T) []byte {
	t.Helper()
	g := &schemaGen{docs: fieldDocs(t), enums: schemaEnums()}
	s := g.typ(reflect.TypeOf(Repo{}), "")
	s["$schema"] = "http://json-schema.org/draft-07/schema#"
	s["title"] = ".reviewbot.yaml"
	s["description"] = "Per-repository settings of the GitLab review bot, read from the MR's target branch. " +
		"Generated from internal/config (UPDATE_SCHEMA=1 go test ./internal/config -run TestRepoSchemaUpToDate); reference: docs/configuration.md."
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestRepoSchemaUpToDate fails when docs/reviewbot.schema.json does not match
// the Repo structs (UPDATE_SCHEMA=1 rewrites it).
func TestRepoSchemaUpToDate(t *testing.T) {
	got := generateRepoSchema(t)
	if os.Getenv("UPDATE_SCHEMA") != "" {
		if err := os.WriteFile(repoSchemaPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(repoSchemaPath)
	if err != nil {
		t.Fatalf("%v (generate it: UPDATE_SCHEMA=1 go test ./internal/config -run TestRepoSchemaUpToDate)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale: regenerate with UPDATE_SCHEMA=1 go test ./internal/config -run TestRepoSchemaUpToDate", repoSchemaPath)
	}
}

// TestRepoSchemaShape checks the generator on known keys.
func TestRepoSchemaShape(t *testing.T) {
	var s struct {
		AdditionalProperties bool                       `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(generateRepoSchema(t), &s); err != nil {
		t.Fatal(err)
	}
	if s.AdditionalProperties {
		t.Fatal("unknown top-level keys must be rejected")
	}
	for _, k := range yamlKeyPaths(reflect.TypeOf(Repo{}), "") {
		if !strings.Contains(k, ".") && !strings.Contains(k, "[") && s.Properties[k] == nil {
			t.Errorf("schema misses %s", k)
		}
	}
	// Removed keys are not in the schema (the editor flags them as unknown;
	// ValidateRepo explains why they are gone).
	for _, k := range removedRepoKeys {
		if s.Properties[k] != nil {
			t.Errorf("removed key %s is in the schema", k)
		}
	}
	for k, want := range map[string]string{
		"lint":         `"codequality"`,
		"enabled":      `"boolean"`,
		"limits":       `"pattern"`,
		"ignore":       `"array"`,
		"language":     `"description"`,
		"todo":         `"nit"`,
		"max_mr_lines": `"integer"`,
		"migrations":   `"postgres"`,
	} {
		if !strings.Contains(string(s.Properties[k]), want) {
			t.Errorf("%s: %s lacks %s", k, s.Properties[k], want)
		}
	}
}
