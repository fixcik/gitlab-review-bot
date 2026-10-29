package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docsConfigPath is the configuration reference that must list every key.
const docsConfigPath = "../../docs/configuration.md"

// Markers in docs/configuration.md: service keys are looked up between the
// service marker and the repo marker, .reviewbot.yaml keys after the repo
// marker (both configs have keys such as `enabled` or `limits.*`).
const (
	docsServiceMarker = "<!-- docs-sync:service -->"
	docsRepoMarker    = "<!-- docs-sync:repo -->"
)

var inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")

// yamlKeyPaths returns every yaml key path of t: nested structs as
// "a.b", slices of structs as "a[].b" (the slice key itself is listed too).
// Maps and other values are leaves.
func yamlKeyPaths(t reflect.Type, prefix string) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		out = append(out, key)
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Struct:
			out = append(out, yamlKeyPaths(ft, key)...)
		case ft.Kind() == reflect.Slice && elemStruct(ft.Elem()):
			out = append(out, yamlKeyPaths(ft.Elem(), key+"[]")...)
		}
	}
	return out
}

func elemStruct(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// inlineCode returns the set of `inline code` spans in s.
func inlineCode(s string) map[string]bool {
	set := map[string]bool{}
	for _, m := range inlineCodeRe.FindAllStringSubmatch(s, -1) {
		set[strings.TrimSpace(m[1])] = true
	}
	return set
}

// TestDocsListEveryConfigKey fails when a key of the service config or of
// .reviewbot.yaml is missing from docs/configuration.md.
func TestDocsListEveryConfigKey(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash(docsConfigPath))
	if err != nil {
		t.Fatalf("read %s: %v", docsConfigPath, err)
	}
	doc := string(b)
	si, ri := strings.Index(doc, docsServiceMarker), strings.Index(doc, docsRepoMarker)
	if si < 0 || ri < 0 || ri < si {
		t.Fatalf("%s must contain %q followed by %q", docsConfigPath, docsServiceMarker, docsRepoMarker)
	}
	sections := []struct {
		name string
		typ  reflect.Type
		text string
	}{
		{"service config (config.yaml)", reflect.TypeOf(Service{}), doc[si:ri]},
		{"repo config (.reviewbot.yaml)", reflect.TypeOf(Repo{}), doc[ri:]},
	}
	for _, s := range sections {
		have := inlineCode(s.text)
		keys := yamlKeyPaths(s.typ, "")
		if len(keys) == 0 {
			t.Fatalf("%s: no yaml keys found", s.name)
		}
		var missing []string
		for _, k := range keys {
			if !have[k] {
				missing = append(missing, k)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: keys missing from %s (write them as inline code, e.g. `%s`):\n  %s",
				s.name, docsConfigPath, missing[0], strings.Join(missing, "\n  "))
		}
	}
}

// TestYAMLKeyPaths checks the walker itself on a known shape.
func TestYAMLKeyPaths(t *testing.T) {
	got := strings.Join(yamlKeyPaths(reflect.TypeOf(Repo{}), ""), " ")
	for _, want := range []string{"limits.mr_rerun_cooldown", "path_instructions[].path", "lint.disable", "compat.graphql", "jira.project_keys"} {
		if !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("walker misses %q in %q", want, got)
		}
	}
	got = strings.Join(yamlKeyPaths(reflect.TypeOf(Service{}), ""), " ")
	for _, want := range []string{"review.lint.timeout", "review.bot_mention", "review.path_instructions[].instructions", "diag.max_total_mb", "gitlab.sync_interval"} {
		if !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("walker misses %q", want)
		}
	}
}

// The documented default of review.routing.sensitive_paths is the full list
// (not an excerpt), so it must be exactly DefaultRoutingSensitivePaths.
func TestDocsSensitivePathsDefault(t *testing.T) {
	data, err := os.ReadFile(docsConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "| `review.routing.sensitive_paths` |") {
			row = line
			break
		}
	}
	cols := strings.Split(row, " | ")
	if len(cols) < 4 {
		t.Fatalf("row of review.routing.sensitive_paths not found: %q", row)
	}
	var got []string
	for _, m := range inlineCodeRe.FindAllStringSubmatch(cols[2], -1) {
		got = append(got, m[1])
	}
	if !reflect.DeepEqual(got, DefaultRoutingSensitivePaths) {
		t.Fatalf("docs default %q,\nwant %q (DefaultRoutingSensitivePaths)", got, DefaultRoutingSensitivePaths)
	}
}
