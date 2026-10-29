package safefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setup returns a clone dir and an "outside" dir the MR must never reach.
func setup(t *testing.T) (clone, outside string) {
	t.Helper()
	base := t.TempDir()
	clone, outside = filepath.Join(base, "clone"), filepath.Join(base, "outside")
	for _, d := range []string{clone, outside, filepath.Join(clone, ".git", "info")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return clone, outside
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("written outside the clone: %v", names)
	}
}

func TestPrepareDirReplacesMaliciousSymlink(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		clone, outside := setup(t)
		// The MR commits .reviewbot-lint -> ../outside (or an absolute link).
		if err := os.Symlink(outside, filepath.Join(clone, ".reviewbot-lint")); err != nil {
			t.Fatal(err)
		}
		d, err := PrepareDir(clone, ".reviewbot-lint", fresh)
		if err != nil {
			t.Fatalf("fresh=%v: %v", fresh, err)
		}
		fi, err := os.Lstat(d)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("not a real dir: %v %v", fi, err)
		}
		if err := WriteFile(d, "gitleaks.toml", []byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(clone, ".reviewbot-lint", "gitleaks.toml")); err != nil {
			t.Fatal(err)
		}
		assertEmpty(t, outside)
	}
}

func TestPrepareDirRelativeAndDanglingLinks(t *testing.T) {
	clone, outside := setup(t)
	if err := os.Symlink("../outside", filepath.Join(clone, ".reviewbot-compat")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "new"), filepath.Join(clone, ".reviewbot-cache")); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{".reviewbot-compat", ".reviewbot-cache"} {
		if _, err := PrepareDir(clone, n, n == ".reviewbot-compat"); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	assertEmpty(t, outside)
}

func TestPrepareDirFileAndFreshness(t *testing.T) {
	clone, _ := setup(t)
	p := filepath.Join(clone, ".reviewbot-lint")
	if err := os.WriteFile(p, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareDir(clone, ".reviewbot-lint", true); err != nil {
		t.Fatal(err)
	}
	// fresh=false keeps an existing real directory's contents (caches).
	cache, err := PrepareDir(clone, ".reviewbot-cache", false)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cache, "keep"), []byte("1"), 0o644)
	if _, err := PrepareDir(clone, ".reviewbot-cache", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "keep")); err != nil {
		t.Fatal("cache contents lost")
	}
	// fresh=true empties it.
	if _, err := PrepareDir(clone, ".reviewbot-cache", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "keep")); err == nil {
		t.Fatal("fresh dir not emptied")
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../x"} {
		if _, err := PrepareDir(clone, bad, true); err == nil {
			t.Errorf("accepted name %q", bad)
		}
	}
}

// A fresh RemoveAll of a real dir containing a symlink to outside must not
// delete the outside target's contents.
func TestPrepareDirFreshDoesNotFollowInnerLinks(t *testing.T) {
	clone, outside := setup(t)
	os.WriteFile(filepath.Join(outside, "precious"), []byte("1"), 0o644)
	d := filepath.Join(clone, ".reviewbot-compat")
	os.Mkdir(d, 0o755)
	os.Symlink(outside, filepath.Join(d, "old"))
	if _, err := PrepareDir(clone, ".reviewbot-compat", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Fatal("outside file removed")
	}
}

// The root itself must not be a symlink.
func TestPrepareDirRootSymlinkRejected(t *testing.T) {
	clone, _ := setup(t)
	link := filepath.Join(filepath.Dir(clone), "link")
	os.Symlink(clone, link)
	if _, err := PrepareDir(link, ".reviewbot-lint", true); err == nil {
		t.Fatal("symlinked root accepted")
	}
}

// Files below the scratch dir: a planted intermediate link is refused.
func TestWriteFileRefusesEscape(t *testing.T) {
	clone, outside := setup(t)
	d, err := PrepareDir(clone, ".reviewbot-compat", true)
	if err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(d, "new"), 0o755)
	os.Symlink(outside, filepath.Join(d, "new", "api"))
	if err := WriteFile(d, "new/api/openapi.yaml", []byte("x")); err == nil {
		t.Fatal("write through an escaping link succeeded")
	}
	if err := WriteFile(d, "../../outside/x", []byte("x")); err == nil {
		t.Fatal("write with .. succeeded")
	}
	assertEmpty(t, outside)
	if err := WriteFile(d, "old/a/b/c.proto", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(d, "old", "a", "b", "c.proto")); string(b) != "ok" {
		t.Fatal("nested write failed")
	}
}

func TestExcludeFromGitRefusesEscape(t *testing.T) {
	clone, outside := setup(t)
	ExcludeFromGit(clone, "/.reviewbot-lint/")
	ExcludeFromGit(clone, "/.reviewbot-lint/")
	b, err := os.ReadFile(filepath.Join(clone, ".git", "info", "exclude"))
	if err != nil || strings.Count(string(b), "/.reviewbot-lint/") != 1 {
		t.Fatalf("exclude = %q, %v", b, err)
	}

	// .git/info/exclude -> outside file: not followed.
	clone2, outside2 := setup(t)
	target := filepath.Join(outside2, "victim")
	os.Symlink(target, filepath.Join(clone2, ".git", "info", "exclude"))
	ExcludeFromGit(clone2, "/.reviewbot-cache/")
	assertEmpty(t, outside2)

	// .git/info -> outside dir.
	clone3, outside3 := setup(t)
	os.RemoveAll(filepath.Join(clone3, ".git", "info"))
	os.Symlink(outside3, filepath.Join(clone3, ".git", "info"))
	ExcludeFromGit(clone3, "/.reviewbot-cache/")
	assertEmpty(t, outside3)
	assertEmpty(t, outside)
}
