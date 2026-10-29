package safefs

import (
	"os"
	"path/filepath"
	"testing"
)

// Names that are not a single plain component are refused before touching
// the filesystem: a NUL byte or a Windows separator included.
func TestPrepareDirRejectsSeparatorsAndNUL(t *testing.T) {
	clone, _ := setup(t)
	for _, bad := range []string{"a\\b", `..\x`, "a\x00b", "\x00"} {
		if _, err := PrepareDir(clone, bad, true); err == nil {
			t.Errorf("accepted name %q", bad)
		}
	}
}

func TestPrepareDirMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := PrepareDir(missing, ".reviewbot-lint", true); err == nil {
		t.Fatal("missing root accepted")
	}
	if _, err := os.Lstat(missing); err == nil {
		t.Fatal("missing root was created")
	}
}

// A final path component that is a symlink to a file outside the root is
// refused, not followed.
func TestWriteFileRefusesFinalSymlink(t *testing.T) {
	clone, outside := setup(t)
	d, err := PrepareDir(clone, ".reviewbot-lint", true)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim")
	if err := os.Symlink(victim, filepath.Join(d, "gitleaks.toml")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(d, "gitleaks.toml", []byte("x")); err == nil {
		t.Fatal("write through a final symlink succeeded")
	}
	assertEmpty(t, outside)
}

// An existing exclude file keeps its content; the line is appended on its own
// line (a missing trailing newline is added first) and not duplicated when
// already present (surrounding whitespace ignored).
func TestExcludeFromGitAppendsToExisting(t *testing.T) {
	clone, _ := setup(t)
	excl := filepath.Join(clone, ".git", "info", "exclude")
	if err := os.WriteFile(excl, []byte("# comment\n*.log"), 0o644); err != nil {
		t.Fatal(err)
	}
	ExcludeFromGit(clone, "/.reviewbot-cache/")
	b, _ := os.ReadFile(excl)
	if want := "# comment\n*.log\n/.reviewbot-cache/\n"; string(b) != want {
		t.Fatalf("exclude = %q, want %q", b, want)
	}

	if err := os.WriteFile(excl, []byte("  /.reviewbot-lint/  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ExcludeFromGit(clone, "/.reviewbot-lint/")
	b, _ = os.ReadFile(excl)
	if string(b) != "  /.reviewbot-lint/  \n" {
		t.Fatalf("line duplicated: %q", b)
	}

	// Existing content that already ends with a newline gets no blank line.
	if err := os.WriteFile(excl, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ExcludeFromGit(clone, "/x/")
	b, _ = os.ReadFile(excl)
	if string(b) != "a\n/x/\n" {
		t.Fatalf("exclude = %q", b)
	}

	// A one-byte file without a newline still gets one before the line.
	if err := os.WriteFile(excl, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	ExcludeFromGit(clone, "/y/")
	b, _ = os.ReadFile(excl)
	if string(b) != "a\n/y/\n" {
		t.Fatalf("exclude = %q", b)
	}

	// An empty or missing file gets just the line, no leading blank line.
	os.Remove(excl)
	ExcludeFromGit(clone, "/z/")
	b, _ = os.ReadFile(excl)
	if string(b) != "/z/\n" {
		t.Fatalf("exclude = %q", b)
	}
}

// A clone without a .git directory (or with .git as a file/link) is left
// alone.
func TestExcludeFromGitNoGitDir(t *testing.T) {
	base := t.TempDir()
	ExcludeFromGit(base, "/x/")
	if _, err := os.Lstat(filepath.Join(base, ".git")); err == nil {
		t.Fatal(".git created")
	}
}
