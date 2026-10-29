package initwiz

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// configTemplate is a new config.yaml (the same values as
// deploy/config.compose.yaml; a test keeps them equal).
//
//go:embed config.template.yaml
var configTemplate []byte

// RepoTemplate is the starter .reviewbot.yaml of `init --repo-config`.
//
//go:embed repo.template.yaml
var RepoTemplate []byte

// setting is one config key init owns: a dotted path and its value.
type setting struct {
	path  []string
	value any
}

// editConfig sets the keys in the YAML document src (a new document from
// the template when src is empty): existing keys are replaced in place,
// missing ones added under their parent; other keys and comments stay.
func editConfig(src []byte, sets []setting) ([]byte, error) {
	if strings.TrimSpace(string(src)) == "" {
		src = configTemplate
	}
	f, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("config.yaml: %w", err)
	}
	if len(f.Docs) != 1 || f.Docs[0].Body == nil {
		return nil, fmt.Errorf("config.yaml: expected one YAML document with keys")
	}
	for _, s := range sets {
		// JSON is valid YAML: strings come out quoted, lists in flow style.
		v, _ := json.Marshal(s.value)
		if err := setPath(f, s.path, string(v)); err != nil {
			return nil, fmt.Errorf("config.yaml: %s: %w", strings.Join(s.path, "."), err)
		}
	}
	return []byte(f.String()), nil
}

// yamlPath builds "$.a.b" (init's keys are plain identifiers).
func yamlPath(keys []string) *yaml.Path {
	p, _ := yaml.PathString("$" + strings.Join(append([]string{""}, keys...), "."))
	return p
}

func setPath(f *ast.File, keys []string, value string) error {
	if _, err := yamlPath(keys).FilterFile(f); err == nil {
		return yamlPath(keys).ReplaceWithReader(f, strings.NewReader(value))
	}
	// The deepest existing parent gets the rest as a nested mapping.
	i := len(keys) - 1
	for ; i > 0; i-- {
		if _, err := yamlPath(keys[:i]).FilterFile(f); err == nil {
			break
		}
	}
	var b strings.Builder
	for d, k := range keys[i:] {
		b.WriteString(strings.Repeat("  ", d) + k + ":")
		if d == len(keys[i:])-1 {
			b.WriteString(" " + value)
		}
		b.WriteString("\n")
	}
	return yamlPath(keys[:i]).MergeFromReader(f, strings.NewReader(b.String()))
}

// secretLine: a config line that may hold a secret (an old config with
// gitlab.token, a Jira token, ...). Its value is masked in the diff.
var secretLine = regexp.MustCompile(`(?i)^(\s*[-\w]*(token|secret|password|passwd|api_key|apikey)[-\w]*\s*:\s*)(\S.*)$`)

func maskLine(s string) string {
	if m := secretLine.FindStringSubmatch(s); m != nil {
		return m[1] + "***"
	}
	return s
}

// diffLines is a line diff of a and b ("- " removed, "+ " added), with
// secrets masked; empty when equal. Common lines are left out.
func diffLines(a, b string) string {
	x, y := strings.Split(strings.TrimRight(a, "\n"), "\n"), strings.Split(strings.TrimRight(b, "\n"), "\n")
	// LCS table (config files are small).
	n, m := len(x), len(y)
	l := make([][]int, n+1)
	for i := range l {
		l[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				l[i][j] = l[i+1][j+1] + 1
			} else {
				l[i][j] = max(l[i+1][j], l[i][j+1])
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			i, j = i+1, j+1
		case i < n && (j == m || l[i+1][j] >= l[i][j+1]):
			out.WriteString("- " + maskLine(x[i]) + "\n")
			i++
		default:
			out.WriteString("+ " + maskLine(y[j]) + "\n")
			j++
		}
	}
	return out.String()
}
