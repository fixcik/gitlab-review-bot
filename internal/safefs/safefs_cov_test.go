package safefs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// procDir is this process's /proc directory: a real directory on a file
// system that refuses to create or remove anything, even for root. It
// stands in for a read-only clone (permission bits do not stop root).
func procDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join("/proc", strconv.Itoa(os.Getpid()))
	if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
		t.Skip("no procfs")
	}
	return d
}

// File system errors of PrepareDir are reported, never ignored: a name the
// OS refuses, a non-directory that cannot be removed, a directory that cannot
// be emptied, a directory that cannot be created.
func TestPrepareDirFileSystemErrors(t *testing.T) {
	clone, _ := setup(t)
	if _, err := PrepareDir(clone, strings.Repeat("a", 300), true); err == nil {
		t.Error("over-long name accepted")
	}
	proc := procDir(t)
	for _, tc := range []struct {
		root, name string
		fresh      bool
		want       string
	}{
		{proc, "cwd", false, "remove cwd"}, // a symlink that cannot be removed
		{proc, "fd", true, "clean fd"},     // a directory that cannot be emptied
		{proc, "reviewbot-x", false, "create reviewbot-x"},
	} {
		_, err := PrepareDir(tc.root, tc.name, tc.fresh)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("PrepareDir(%s, %s, %v) = %v, want %q", tc.root, tc.name, tc.fresh, err, tc.want)
		}
	}
	// The kept-as-is case still works on such a root: an existing real
	// directory without fresh is returned untouched.
	if d, err := PrepareDir(proc, "fd", false); err != nil || d != filepath.Join(proc, "fd") {
		t.Errorf("existing dir = %q, %v", d, err)
	}
}

// within accepts only a path that resolves exactly to the expected location
// inside the root: a link leaving the root, a link to another place inside
// it and a missing path are all refused.
func TestWithin(t *testing.T) {
	clone, outside := setup(t)
	realRoot, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(clone, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Symlink(outside, filepath.Join(clone, "out"))
	os.Symlink("dir", filepath.Join(clone, "alias"))
	os.Symlink("..", filepath.Join(clone, "up"))
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"dir", true},
		{"out", false},
		{"alias", false}, // inside the clone, but not the directory asked for
		{"up", false},    // the clone's parent
		{"missing", false},
	} {
		err := within(realRoot, filepath.Join(clone, tc.name), filepath.Join(realRoot, tc.name))
		if (err == nil) != tc.ok {
			t.Errorf("within(%s) = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	// Even when the caller expects the outside path, a target outside the
	// root is refused.
	if err := within(realRoot, filepath.Join(clone, "up"), filepath.Dir(realRoot)); err == nil {
		t.Error("parent of the root accepted")
	}
}

// WriteFile and MkdirAll need an existing root; a file in the way of a
// parent directory is an error, not replaced or followed; a failing write is
// reported.
func TestWriteFileAndMkdirAllErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := WriteFile(missing, "a", []byte("x")); err == nil {
		t.Error("WriteFile into a missing root succeeded")
	}
	if err := MkdirAll(missing, "a/b"); err == nil {
		t.Error("MkdirAll in a missing root succeeded")
	}
	if _, err := os.Lstat(missing); err == nil {
		t.Fatal("missing root created")
	}

	clone, _ := setup(t)
	if err := os.WriteFile(filepath.Join(clone, "file"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func() error{
		func() error { return WriteFile(clone, "file/x", []byte("x")) },
		func() error { return MkdirAll(clone, "file/x/y") },
	} {
		if err := f(); err == nil || !strings.Contains(err.Error(), "is not a directory") {
			t.Errorf("err = %v, want not a directory", err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(clone, "file")); string(b) != "keep" {
		t.Errorf("file changed: %q", b)
	}
	if err := MkdirAll(clone, "a/b/c"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(clone, "a", "b", "c")); err != nil || !fi.IsDir() {
		t.Fatalf("a/b/c: %v", err)
	}
	if err := MkdirAll(clone, "."); err != nil {
		t.Errorf("MkdirAll(.) = %v", err)
	}

	// /dev/full accepts the open and fails every write (ENOSPC).
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("no /dev/full")
	}
	if err := WriteFile("/dev", "full", []byte("x")); err == nil {
		t.Error("failed write not reported")
	}
}

// ExcludeFromGit is best effort and never writes through what the clone
// planted: a missing clone, an exclude that is a directory, a dangling
// .git/info link and a dangling exclude link leave everything as it was.
func TestExcludeFromGitBestEffort(t *testing.T) {
	ExcludeFromGit(filepath.Join(t.TempDir(), "missing"), "/x/") // no panic, nothing created

	// exclude is a directory: reading fails, nothing is appended.
	clone, _ := setup(t)
	excl := filepath.Join(clone, ".git", "info", "exclude")
	if err := os.Mkdir(excl, 0o755); err != nil {
		t.Fatal(err)
	}
	ExcludeFromGit(clone, "/x/")
	if ents, _ := os.ReadDir(excl); len(ents) != 0 {
		t.Errorf("exclude dir changed: %v", ents)
	}

	// .git/info is a dangling link inside the clone: the link target is
	// not created.
	clone2, _ := setup(t)
	info := filepath.Join(clone2, ".git", "info")
	os.RemoveAll(info)
	if err := os.Symlink("../gone", info); err != nil {
		t.Fatal(err)
	}
	ExcludeFromGit(clone2, "/x/")
	if _, err := os.Lstat(filepath.Join(clone2, "gone")); err == nil {
		t.Error("link target of .git/info created")
	}

	// exclude is a dangling link into a missing directory of the clone:
	// neither the directory nor the file appear.
	clone3, _ := setup(t)
	if err := os.Symlink("../../nodir/x", filepath.Join(clone3, ".git", "info", "exclude")); err != nil {
		t.Fatal(err)
	}
	ExcludeFromGit(clone3, "/x/")
	if _, err := os.Lstat(filepath.Join(clone3, "nodir")); err == nil {
		t.Error("link target directory created")
	}
}

// A symlinked root is refused before anything is created through it.
func TestPrepareDirSymlinkRootCreatesNothing(t *testing.T) {
	clone, _ := setup(t)
	link := filepath.Join(filepath.Dir(clone), "link")
	if err := os.Symlink(clone, link); err != nil {
		t.Fatal(err)
	}
	for _, fresh := range []bool{true, false} {
		if _, err := PrepareDir(link, ".reviewbot-lint", fresh); err == nil {
			t.Fatal("symlinked root accepted")
		}
		if _, err := os.Lstat(filepath.Join(clone, ".reviewbot-lint")); err == nil {
			t.Fatal("directory created through the symlinked root")
		}
	}
}
