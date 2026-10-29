package repo

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Entries named .git anywhere in the path (any case) are never extracted:
// a snapshot cannot carry git hooks or config.
func TestExtractTarSkipsGitDirsAnywhere(t *testing.T) {
	root := t.TempDir()
	if _, err := extractTar(buildTar(t, []tarEntry{
		{name: "sub/.GIT/hooks/pre-commit", typ: tar.TypeReg, mode: 0o755, body: "evil"},
		{name: "a/.Git", typ: tar.TypeSymlink, link: "/"},
		{name: "keep.txt", typ: tar.TypeReg, mode: 0o644, body: "ok"},
	}), root, 0); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"sub", "a"} {
		if _, err := os.Lstat(filepath.Join(root, p)); err == nil {
			t.Errorf("%s extracted", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "keep.txt")); string(b) != "ok" {
		t.Errorf("keep.txt = %q", b)
	}
}

// A file where a directory is needed, a truncated file body and a link
// name the OS refuses are errors, not silently skipped.
func TestExtractTarFileSystemErrors(t *testing.T) {
	file := tarEntry{name: "a", typ: tar.TypeReg, mode: 0o644, body: "x"}
	for name, entries := range map[string][]tarEntry{
		"dir under a file":  {file, {name: "a/b/", typ: tar.TypeDir, mode: 0o755}},
		"file under a file": {file, {name: "a/b", typ: tar.TypeReg, mode: 0o644, body: "y"}},
		"link under a file": {file, {name: "a/l", typ: tar.TypeSymlink, link: "x"}},
		"over-long link":    {{name: strings.Repeat("l", 300), typ: tar.TypeSymlink, link: "x"}},
	} {
		if _, err := extractTar(buildTar(t, entries), t.TempDir(), 0); err == nil || !strings.HasPrefix(err.Error(), "repo: ") {
			t.Errorf("%s: %v", name, err)
		}
	}

	// The archive ends in the middle of a file body.
	full := buildTar(t, []tarEntry{{name: "big", typ: tar.TypeReg, mode: 0o644, body: strings.Repeat("z", 2000)}}).Bytes()
	root := t.TempDir()
	_, err := extractTar(bytes.NewReader(full[:512+100]), root, 0)
	if err == nil || !strings.Contains(err.Error(), "write big") || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated: %v", err)
	}
}

// outsideTree treats a path it cannot read as a link as outside (removed).
func TestOutsideTreeUnreadableLink(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "plain")
	os.WriteFile(f, []byte("x"), 0o644)
	if !outsideTree(root, f) || !outsideTree(root, filepath.Join(root, "missing")) {
		t.Fatal("unreadable link judged inside")
	}
}

// A missing root removes nothing; links that cannot be removed are counted
// as kept and logged (with or without a logger).
func TestNeutralizeSymlinksFailures(t *testing.T) {
	if n := neutralizeSymlinks(filepath.Join(t.TempDir(), "missing"), nil); n != 0 {
		t.Fatalf("missing root: %d", n)
	}
	// /proc/<pid>/fd: links to files outside it (stdin, pipes) on a file
	// system that refuses to remove anything.
	fdDir := filepath.Join("/proc", strconv.Itoa(os.Getpid()), "fd")
	if _, err := os.Stat(fdDir); err != nil {
		t.Skip("no procfs")
	}
	var logs bytes.Buffer
	if n := neutralizeSymlinks(fdDir, slog.New(slog.NewTextHandler(&logs, nil))); n != 0 {
		t.Fatalf("removed %d", n)
	}
	if !strings.Contains(logs.String(), "failed to remove out-of-tree symlink") {
		t.Fatalf("log: %s", logs.String())
	}
	if n := neutralizeSymlinks(fdDir, nil); n != 0 {
		t.Fatalf("removed %d", n)
	}
}

// exitCode is the git exit code of a (wrapped) gitError, else -1; a git
// that cannot be started fails stream without calling fn.
func TestExitCodeAndStreamStartFailure(t *testing.T) {
	if exitCode(errors.New("x")) != -1 || exitCode(fmt.Errorf("w: %w", &gitError{exitCode: 128})) != 128 {
		t.Fatal("exitCode")
	}
	g := &gitRunner{bin: filepath.Join(t.TempDir(), "no-git")}
	called := false
	err := g.stream(context.Background(), gitCmd{dir: t.TempDir(), args: []string{"archive", "HEAD"}}, func(io.Reader) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}
