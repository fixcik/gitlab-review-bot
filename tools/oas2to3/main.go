// Command oas2to3 converts a Swagger 2.0 document to OpenAPI 3.0 so that
// oasdiff (which only understands OpenAPI 3.x and silently reports nothing
// for Swagger 2.0) can diff it. It runs inside the review sandbox, without
// network, on files from the merge request.
//
//	oas2to3 <in.yaml|in.json> <out.yaml|out.json>
//
// External $refs are not loaded (the document is only unmarshalled);
// local refs (#/definitions/...) are rewritten to #/components/...
// Exit codes: 0 ok, 2 usage, 3 the input is not Swagger 2.0, 1 other errors.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/oasdiff/yaml"
)

const maxInput = 32 << 20

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: oas2to3 <in> <out>")
		os.Exit(2)
	}
	os.Exit(run(os.Args[1], os.Args[2]))
}

func run(in, out string) int {
	fi, err := os.Stat(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if fi.Size() > maxInput {
		fmt.Fprintf(os.Stderr, "%s: larger than %d MB\n", in, maxInput>>20)
		return 1
	}
	b, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// YAML is a superset of JSON: one path for both.
	jb, err := yaml.YAMLToJSON(b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: parse: %v\n", in, err)
		return 1
	}
	var head struct {
		Swagger string `json:"swagger"`
	}
	if err := json.Unmarshal(jb, &head); err != nil || !strings.HasPrefix(strings.TrimSpace(head.Swagger), "2") {
		fmt.Fprintf(os.Stderr, "%s: not a Swagger 2.0 document\n", in)
		return 3
	}
	var doc2 openapi2.T
	if err := json.Unmarshal(jb, &doc2); err != nil {
		fmt.Fprintf(os.Stderr, "%s: swagger 2.0: %v\n", in, err)
		return 1
	}
	doc3, err := openapi2conv.ToV3(&doc2)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: convert: %v\n", in, err)
		return 1
	}
	ob, err := json.MarshalIndent(doc3, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch strings.ToLower(filepath.Ext(out)) {
	case ".yaml", ".yml":
		if ob, err = yaml.JSONToYAML(ob); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if err := os.WriteFile(out, ob, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
