package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/oasdiff/yaml"
)

const swagger2 = `swagger: "2.0"
info: {title: Orders, version: "1.0"}
basePath: /api
definitions:
  Order:
    type: object
    required: [currency]
    properties:
      currency: {type: string}
paths:
  /orders:
    post:
      consumes: [application/json]
      produces: [application/json]
      parameters:
        - in: body
          name: body
          required: true
          schema: {$ref: "#/definitions/Order"}
      responses:
        "200":
          description: ok
          schema:
            type: object
            properties:
              total: {type: string}
`

func TestRun(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "swagger.yaml")
	if err := os.WriteFile(in, []byte(swagger2), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{"out.yaml", "out.json"} {
		p := filepath.Join(dir, out)
		if code := run(in, p); code != 0 {
			t.Fatalf("%s: exit %d", out, code)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		jb, err := yaml.YAMLToJSON(b)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			OpenAPI string `json:"openapi"`
			Paths   map[string]struct {
				Post struct {
					RequestBody struct {
						Content map[string]struct {
							Schema struct {
								Ref string `json:"$ref"`
							} `json:"schema"`
						} `json:"content"`
					} `json:"requestBody"`
				} `json:"post"`
			} `json:"paths"`
			Components struct {
				Schemas map[string]any `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(jb, &doc); err != nil {
			t.Fatal(err)
		}
		ref := doc.Paths["/orders"].Post.RequestBody.Content["application/json"].Schema.Ref
		if doc.OpenAPI != "3.0.3" || ref != "#/components/schemas/Order" || doc.Components.Schemas["Order"] == nil {
			t.Errorf("%s: openapi=%q ref=%q\n%s", out, doc.OpenAPI, ref, b)
		}
	}
}

func TestRunExitCodes(t *testing.T) {
	dir := t.TempDir()
	oas3 := filepath.Join(dir, "openapi.yaml")
	if err := os.WriteFile(oas3, []byte("openapi: 3.0.3\ninfo: {title: x, version: '1'}\npaths: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run(oas3, filepath.Join(dir, "o.yaml")); code != 3 {
		t.Errorf("not swagger 2.0: exit %d", code)
	}
	if code := run(filepath.Join(dir, "missing.yaml"), filepath.Join(dir, "o.yaml")); code != 1 {
		t.Errorf("missing input: exit %d", code)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("swagger: '2.0'\npaths: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run(bad, filepath.Join(dir, "o.yaml")); code != 1 {
		t.Errorf("unparsable: exit %d", code)
	}
}
