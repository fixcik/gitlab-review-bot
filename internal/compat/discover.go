package compat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Built-in discovery globs (doublestar, matched against the repo path).
var (
	openAPIGlobs = []string{
		"**/openapi*.yaml", "**/openapi*.yml", "**/openapi*.json",
		"**/swagger*.yaml", "**/swagger*.yml", "**/swagger*.json",
	}
	protoGlobs   = []string{"**/*.proto"}
	graphQLGlobs = []string{"**/*.graphql", "**/*.graphqls", "**/schema.gql"}
)

// Discover picks the spec files among changed paths. read returns a file's
// content (head version, or base for deleted files) for content sniffing,
// nil when unavailable. Paths inside the bot's own scratch/cache dirs are
// ignored. The result is sorted by path; a file gets at most one kind.
func Discover(changed []string, cfg Config, read func(string) []byte) []Spec {
	var out []Spec
	seen := map[string]bool{}
	for _, p := range changed {
		if p == "" || seen[p] || botPath(p) {
			continue
		}
		seen[p] = true
		if k := kindOf(p, cfg, read); k != "" {
			out = append(out, Spec{Kind: k, Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func botPath(p string) bool {
	return strings.HasPrefix(p, ScratchDir+"/") || strings.HasPrefix(p, ".reviewbot-cache/")
}

func kindOf(p string, cfg Config, read func(string) []byte) string {
	low := strings.ToLower(p)
	switch {
	case matchAny(cfg.OpenAPI, p):
		return KindOpenAPI
	case matchAny(cfg.Proto, p):
		return KindProto
	case matchAny(cfg.GraphQL, p):
		return KindGraphQL
	case matchAny(protoGlobs, low):
		return KindProto
	case matchAny(openAPIGlobs, low):
		return KindOpenAPI
	case matchAny(graphQLGlobs, low):
		if read == nil {
			return KindGraphQL
		}
		if b := read(p); b == nil || LooksLikeGraphQLSchema(b) {
			return KindGraphQL
		}
		return "" // an operations document, not a schema
	}
	switch path.Ext(low) {
	case ".yaml", ".yml", ".json":
		if read != nil && LooksLikeOpenAPI(read(p)) {
			return KindOpenAPI
		}
	}
	return ""
}

// matchAny matches p (the whole repository path) against doublestar globs:
// "**/api.yaml" finds it at any depth, the root included; "api.yaml" only at
// the root. Invalid globs match nothing.
func matchAny(globs []string, p string) bool {
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if ok, err := doublestar.Match(g, p); err == nil && ok {
			return true
		}
	}
	return false
}

var yamlTopKeyRe = regexp.MustCompile(`^(openapi|swagger|"openapi"|"swagger"|'openapi'|'swagger')\s*:(.*)$`)

// LooksLikeOpenAPI reports whether a YAML or JSON document has a top-level
// "openapi" or "swagger" key.
func LooksLikeOpenAPI(b []byte) bool {
	key, _ := SpecVersion(b)
	return key != ""
}

// SpecVersion returns the top-level "openapi" or "swagger" key of a YAML or
// JSON document and its value ("swagger", "2.0"; "openapi", "3.1.0"); key is
// "" when there is none.
func SpecVersion(b []byte) (key, version string) {
	t := bytes.TrimSpace(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")))
	if len(t) == 0 {
		return "", ""
	}
	if t[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(t))
		_, _ = dec.Token() // the opening '{' (t starts with it)
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return "", ""
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return "", ""
			}
			if k, ok := tok.(string); ok && (k == "openapi" || k == "swagger") {
				var v any
				_ = json.Unmarshal(raw, &v)
				switch x := v.(type) {
				case string:
					return k, strings.TrimSpace(x)
				case float64:
					return k, string(bytes.TrimSpace(raw))
				}
				return k, ""
			}
		}
		return "", ""
	}
	sc := bufio.NewScanner(bytes.NewReader(t))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		// Top-level keys start at column 0; comments, documents markers and
		// indented lines are not top-level keys.
		m := yamlTopKeyRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		v := m[2]
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		return strings.Trim(m[1], `"'`), strings.TrimSpace(v)
	}
	return "", ""
}

var graphQLDefRe = regexp.MustCompile(`(?m)^\s*(?:"""[^"]*"""\s*)?(?:extend\s+)?(?:schema|type|input|enum|interface|union|scalar|directive)\b`)

// LooksLikeGraphQLSchema reports whether a .graphql file defines types (a
// schema), as opposed to holding only operations and fragments.
func LooksLikeGraphQLSchema(b []byte) bool {
	return graphQLDefRe.Match(b)
}
