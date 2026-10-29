package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRepoConfigCLI(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runValidateRepoConfig(args, &out, &errb)
		return code, out.String(), errb.String()
	}

	ok := write("ok.yaml", "# yaml-language-server: $schema=x\nlanguage: ru\n")
	if code, out, _ := run(ok); code != 0 || !strings.Contains(out, "ok.yaml: ok") {
		t.Fatalf("valid: %d %q", code, out)
	}
	bad := write("bad.yaml", "languag: ru\nlint:\n  disable: [nope]\nmax_files: x\n")
	code, out, errs := run(bad)
	if code != 1 {
		t.Fatalf("invalid: exit %d", code)
	}
	for _, want := range []string{bad + ":1: languag: unknown key", bad + ":2: lint.disable: unknown tool", bad + ":4: max_files:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errs, "3 error(s)") {
		t.Errorf("stderr %q", errs)
	}
	if code, _, _ := run(filepath.Join(dir, "missing.yaml")); code != 2 {
		t.Fatalf("missing file: exit %d", code)
	}
	if code, _, _ := run("a", "b"); code != 2 {
		t.Fatalf("two args: exit %d", code)
	}
	// Default path: ./.reviewbot.yaml.
	t.Chdir(dir)
	write(".reviewbot.yaml", "")
	if code, out, _ := run(); code != 0 || !strings.Contains(out, ".reviewbot.yaml: ok") {
		t.Fatalf("default path: %d %q", code, out)
	}
}

// The example shipped in the repository is valid for the CLI too.
func TestValidateRepoConfigExample(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runValidateRepoConfig([]string{filepath.Join("..", "..", ".reviewbot.example.yaml")}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
}
