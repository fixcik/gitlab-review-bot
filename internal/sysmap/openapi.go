package sysmap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
)

// maxSpecEntries caps the operations taken from one spec.
const maxSpecEntries = 2000

// specMethods are the operation keys of an OpenAPI path item.
var specMethods = map[string]bool{"get": true, "put": true, "post": true, "delete": true, "options": true, "head": true, "patch": true, "trace": true}

// ParseSpec returns the HTTP endpoints an OpenAPI 3.x or Swagger 2.0
// document provides, with the line of each operation. It never resolves
// $ref: a path item that is only a $ref yields one entry with an unknown
// method. Swagger 2.0 basePath is prefixed to every path. YAML and JSON are
// both read (JSON is YAML).
func ParseSpec(file string, b []byte) ([]Entry, error) {
	key, _ := compat.SpecVersion(b)
	if key == "" {
		return nil, fmt.Errorf("not an OpenAPI/Swagger document")
	}
	f, err := parser.ParseBytes(b, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	if len(f.Docs) == 0 || f.Docs[0] == nil {
		return nil, nil
	}
	root := mapValues(f.Docs[0].Body)
	base := ""
	if key == "swagger" {
		if v := scalar(lookup(root, "basePath")); v != "" && v != "/" {
			base = "/" + strings.Trim(v, "/")
		}
	}
	var out []Entry
	for _, pv := range mapValues(valueOf(lookup(root, "paths"))) {
		raw := keyText(pv.Key)
		if !strings.HasPrefix(raw, "/") {
			continue // extensions (x-...) and junk
		}
		p := NormalizePath(base + raw)
		if p == "" {
			continue
		}
		line := lineOf(pv.Key)
		ops := mapValues(pv.Value)
		found := false
		for _, op := range ops {
			m := strings.ToLower(keyText(op.Key))
			if !specMethods[m] {
				continue
			}
			found = true
			out = append(out, Entry{
				Side: Provides, Protocol: HTTP, Method: strings.ToUpper(m), Path: p, Raw: clip(base+raw, maxRawRunes),
				File: file, Line: lineOf(op.Key), Source: SourceOpenAPI,
				Note: clip(opNote(op.Value), maxNoteRunes),
			})
			if len(out) >= maxSpecEntries {
				return out, nil
			}
		}
		if !found && lookup(ops, "$ref") != nil {
			out = append(out, Entry{Side: Provides, Protocol: HTTP, Path: p, Raw: clip(base+raw, maxRawRunes),
				File: file, Line: line, Source: SourceOpenAPI, Note: "$ref (не раскрывается)"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out, nil
}

// opNote is the operation's operationId or summary.
func opNote(n ast.Node) string {
	vs := mapValues(n)
	if id := scalar(lookup(vs, "operationId")); id != "" {
		return id
	}
	return scalar(lookup(vs, "summary"))
}

// mapValues returns the key/value pairs of a mapping node (nil otherwise).
func mapValues(n ast.Node) []*ast.MappingValueNode {
	switch v := n.(type) {
	case *ast.MappingNode:
		return v.Values
	case *ast.MappingValueNode:
		return []*ast.MappingValueNode{v}
	case *ast.TagNode:
		return mapValues(v.Value)
	case *ast.AnchorNode:
		return mapValues(v.Value)
	}
	return nil
}

func lookup(vs []*ast.MappingValueNode, key string) *ast.MappingValueNode {
	for _, v := range vs {
		if keyText(v.Key) == key {
			return v
		}
	}
	return nil
}

func valueOf(v *ast.MappingValueNode) ast.Node {
	if v == nil {
		return nil
	}
	return v.Value
}

func keyText(k ast.MapKeyNode) string {
	if k == nil {
		return ""
	}
	if s, ok := k.(ast.ScalarNode); ok {
		return strings.TrimSpace(fmt.Sprint(s.GetValue()))
	}
	return strings.TrimSpace(k.String())
}

func scalar(v *ast.MappingValueNode) string {
	if v == nil || v.Value == nil {
		return ""
	}
	if s, ok := v.Value.(ast.ScalarNode); ok {
		return strings.TrimSpace(fmt.Sprint(s.GetValue()))
	}
	return ""
}

func lineOf(n ast.Node) int {
	if n == nil {
		return 0
	}
	if t := n.GetToken(); t != nil && t.Position != nil {
		return t.Position.Line
	}
	return 0
}
