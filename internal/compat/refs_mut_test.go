package compat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSide(t *testing.T, files map[string]string) string {
	t.Helper()
	d := t.TempDir()
	for p, c := range files {
		full := filepath.Join(d, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// Minified JSON specs hold every $ref on one line: each of them is checked,
// not only the first one.
func TestScanRefsEveryRefOnALine(t *testing.T) {
	for name, tc := range map[string]struct {
		spec string
		want refPolicy
	}{
		"url after local":     {`{"openapi":"3.0.0","a":{"$ref":"s.json#/A"},"c":{"$ref":"http://169.254.169.254/x"}}`, refsRefused},
		"outside after local": {`{"openapi":"3.0.0","a":{"$ref":"s.json#/A"},"b":{"$ref":"../../etc/passwd"}}`, refsRefused},
		"url after internal":  {`{"openapi":"3.0.0","a":{"$ref":"#/components/x"},"c":{"$ref":"https://e.com/a.json"}}`, refsRefused},
		"two local":           {`{"openapi":"3.0.0","a":{"$ref":"#/c/x"},"b":{"$ref":"s.json#/A"}}`, refsLocal},
		"internal only":       {`{"openapi":"3.0.0","a":{"$ref":"#/c/x"},"b":{"$ref":"#/c/y"}}`, refsNone},
		"yaml flow":           {"openapi: 3.0.0\nx: {a: {$ref: './s.json'}, b: {$ref: 'http://x/y'}}\n", refsRefused},
		"value too long":      {`{"a":{"$ref":"` + strings.Repeat("a", maxRefLine) + `.json"}}`, refsRefused},
	} {
		d := writeSide(t, map[string]string{"api/o.json": tc.spec, "api/s.json": `{"A":{}}`})
		if got, why := scanRefs(d, "api/o.json"); got != tc.want {
			t.Errorf("%s: policy %d (%s), want %d", name, got, why, tc.want)
		}
	}
	// Transitively, in a referenced file.
	d := writeSide(t, map[string]string{
		"o.json": `{"openapi":"3.0.0","a":{"$ref":"s.json#/A"}}`,
		"s.json": `{"A":{"$ref":"#/B"},"B":{"$ref":"/etc/passwd"}}`,
	})
	if got, _ := scanRefs(d, "o.json"); got != refsRefused {
		t.Errorf("transitive: policy %d", got)
	}
}

func TestScanRefsLinesAndCycles(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  refPolicy
	}{
		// The value ends at the line end, the next key is not part of it.
		"next line key": {map[string]string{"o.yaml": "openapi: 3.0.0\na:\n  $ref: s.yaml\nb:1\n", "s.yaml": "x: 1\n"}, refsLocal},
		// A value on the next line is not read.
		"value on next line": {map[string]string{"o.yaml": "openapi: 3.0.0\na:\n  $ref:\n    './s.yaml'\n", "s.yaml": "x: 1\n"}, refsRefused},
		// Files referencing each other: the scan terminates.
		"cycle": {map[string]string{"o.yaml": "openapi: 3.0.0\na: {$ref: 'a.yaml'}\n", "a.yaml": "b: {$ref: 'b.yaml#/x'}\n", "b.yaml": "x: {$ref: './a.yaml'}\n"}, refsLocal},
	} {
		d := writeSide(t, tc.files)
		if got, why := scanRefs(d, "o.yaml"); got != tc.want {
			t.Errorf("%s: policy %d (%s), want %d", name, got, why, tc.want)
		}
	}
}
