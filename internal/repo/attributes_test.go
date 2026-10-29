package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// hostileAttributes is a .gitattributes an MR author (or the default
// branch) can commit to hide code from the bot or change what it reads:
// "Binary files differ" instead of the code, files dropped from exports,
// content rewritten on checkout/export.
const hostileAttributes = `[attr]hide -diff -text
*.go -diff
b.go binary
c.go hide
real.bin diff
sec/** export-ignore
sec export-ignore
subst.txt export-subst ident
enc.txt working-tree-encoding=UTF-16LE
crlf.txt eol=crlf
`

// setupHostileMR is a project whose default branch carries hostile
// .gitattributes (also in a subdirectory), and an MR changing the files
// they cover.
func setupHostileMR(t *testing.T) (*remote, *core.MergeRequest) {
	t.Helper()
	r := newRemote(t)
	// The fake GitLab's own git must store the files as written.
	if err := os.WriteFile(filepath.Join(r.dir, ".git", "info", "attributes"), []byte(neutralAttributes), 0o644); err != nil {
		t.Fatal(err)
	}
	r.write(".gitattributes", hostileAttributes)
	r.write("pkg/.gitattributes", "* -diff\n")
	r.write("a.go", "package a\n\nfunc A() {}\n\n// end\n")
	r.write("real.bin", "\x00\x01bin\n")
	r.write("sec/s.go", "package sec\n")
	r.write("pkg/x.go", "package pkg\n")
	r.write("crlf.txt", "l1\nl2\n")
	r.write("subst.txt", "v $Id$ $Format:%H$\n")
	r.write("enc.txt", "hello\n")
	base := r.commit("init")
	// "master": the branch HEAD of a fresh bare mirror points to, whose
	// attributes git >= 2.43 applies in a bare repository.
	gitT(t, r.dir, "branch", "-m", "main", "master")
	gitT(t, r.dir, "checkout", "-q", "-b", "feature")
	r.write("a.go", "package a\n\nfunc A() { evilA() }\n\n// end\n")
	r.write("b.go", "package b\n\nfunc B() { evilB() }\n")
	r.write("c.go", "package c\n\nfunc C() { evilC() }\n")
	r.write("pkg/x.go", "package pkg\n\nfunc X() { evilX() }\n")
	r.write("sec/s.go", "package sec\n\nfunc S() { evilS() }\n")
	r.write("real.bin", "\x00\x02bin2\n")
	head := r.commit("mr")
	r.setMRHead(1, head)
	gitT(t, r.dir, "checkout", "-q", "master")
	return r, &core.MergeRequest{IID: 1, TargetBranch: "master",
		DiffRefs: core.DiffRefs{BaseSHA: base, StartSHA: base, HeadSHA: head}}
}

var evilCode = []string{"+func A() { evilA() }", "+func B() { evilB() }", "+func C() { evilC() }",
	"+func X() { evilX() }", "+func S() { evilS() }"}

// checkRawFiles asserts that dir holds the MR's files exactly as committed:
// nothing export-ignored, nothing re-encoded, converted or expanded.
func checkRawFiles(t *testing.T, what, dir string, head bool) {
	t.Helper()
	want := map[string]string{
		"crlf.txt":  "l1\nl2\n",
		"subst.txt": "v $Id$ $Format:%H$\n",
		"enc.txt":   "hello\n",
		"sec/s.go":  "package sec\n",
	}
	if head {
		want["sec/s.go"] = "package sec\n\nfunc S() { evilS() }\n"
	}
	for p, w := range want {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil || string(b) != w {
			t.Errorf("%s: %s = %q, %v; want %q", what, p, b, err, w)
		}
	}
}

// A repository's .gitattributes cannot hide or alter what the bot sees: the
// diff, changed files, line checks, grep, exported trees and the checkout
// itself are produced as if there were no attributes, in clone mode (the
// MR's own attributes in the working tree) and in cache mode (the bare
// mirror: attributes of its HEAD, i.e. the default branch, on git >= 2.43;
// of the tree being archived or checked out on every version). Real binary
// files are still reported as binary, never dumped into the diff.
func TestRepositoryAttributesIgnored(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "clone"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			r, mr := setupHostileMR(t)
			var m *Manager
			if cached {
				m = newCachedManager(t, filepath.Dir(filepath.Dir(r.dir)), time.Hour, 0)
			} else {
				m = newManager(t, filepath.Dir(filepath.Dir(r.dir)))
			}
			ctx := context.Background()
			co, err := m.Checkout(ctx, "group/proj", mr)
			if err != nil {
				t.Fatal(err)
			}
			defer co.Close()
			c := co.(*Checkout)
			from := mr.DiffRefs.StartSHA

			for _, ignore := range [][]string{nil, {"vendor/**"}} {
				diff, err := co.Diff(ctx, from, ignore)
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range evilCode {
					if !strings.Contains(diff, s) {
						t.Errorf("diff (ignore %v) hides %q:\n%s", ignore, s, diff)
					}
				}
				if n := strings.Count(diff, "Binary files"); n != 1 || !strings.Contains(diff, "Binary files a/real.bin and b/real.bin differ") {
					t.Errorf("diff (ignore %v): want only real.bin as binary (%d):\n%s", ignore, n, diff)
				}
				if strings.ContainsRune(diff, 0) {
					t.Errorf("diff (ignore %v) dumps binary content", ignore)
				}
			}
			files, err := co.ChangedFiles(ctx, from, nil)
			if err != nil || strings.Join(files, ",") != "a.go,b.go,c.go,pkg/x.go,real.bin,sec/s.go" {
				t.Errorf("changed files %v %v", files, err)
			}
			// Line 5 of a.go is untouched: a "Binary files" diff would
			// count as a change.
			if ch, err := c.LinesChanged(ctx, from, "", "a.go", 5, 5); err != nil || ch {
				t.Errorf("a.go:5 changed = %v, %v", ch, err)
			}
			if ch, err := c.LinesChanged(ctx, from, "", "a.go", 3, 3); err != nil || !ch {
				t.Errorf("a.go:3 changed = %v, %v", ch, err)
			}
			ms, _, err := c.GrepWords(ctx, mr.DiffRefs.HeadSHA, []string{"evilA", "evilB", "evilC", "evilX"}, nil, 0)
			if err != nil || len(ms) != 4 {
				t.Errorf("grep found %v, %v", ms, err)
			}

			// The working tree Claude reads.
			checkRawFiles(t, "checkout", co.Dir(), true)
			for _, rev := range []string{mr.DiffRefs.HeadSHA, mr.DiffRefs.BaseSHA} {
				dir, rel, err := c.ExportTree(ctx, rev, 0)
				if err != nil {
					t.Fatal(err)
				}
				checkRawFiles(t, "export of "+shortRev(rev), dir, rev == mr.DiffRefs.HeadSHA)
				rel()
			}

			// Tools reading history in the sandbox (gitleaks) get the same
			// neutral attributes.
			hd, err := c.HistoryDir(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(hd, ".git", "info", "attributes")); err != nil || string(b) != neutralAttributes {
				t.Errorf("history dir attributes %q %v", b, err)
			}
		})
	}
}

// Context repository snapshots are exported without export-ignore,
// export-subst or conversions.
func TestSnapshotIgnoresRepositoryAttributes(t *testing.T) {
	base := t.TempDir()
	r := newGroupRemote(t, base, "lib/core", map[string]string{"README.md": "x\n"})
	r.write(".gitattributes", "hidden.go export-ignore\nv.txt export-subst ident\ncrlf.txt eol=crlf\n")
	r.write("hidden.go", "package hidden\n")
	r.write("v.txt", "$Id$ $Format:%H$\n")
	r.write("crlf.txt", "a\nb\n")
	r.commit("attrs")
	m, _ := newSnapManager(t, base)
	s, err := m.Snapshot(context.Background(), SnapshotSpec{Path: "lib/core", DefaultBranch: "main"}, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	for p, w := range map[string]string{"hidden.go": "package hidden\n", "v.txt": "$Id$ $Format:%H$\n", "crlf.txt": "a\nb\n"} {
		if b, err := os.ReadFile(filepath.Join(s.Dir, p)); err != nil || string(b) != w {
			t.Errorf("%s = %q, %v; want %q", p, b, err, w)
		}
	}
}

// Snapshots exported by an older version (with the repository's
// export-ignore applied) are dropped at startup, not reused for their SHA;
// once marked, the directory's snapshots survive a restart.
func TestSnapshotStoreDropsOldFormat(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "lib_core-1234@abc")
	if err := os.MkdirAll(filepath.Join(old, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := newSnapshotStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old-format snapshot kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.dir, snapshotFormat)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newSnapshotStore(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("current-format snapshot dropped: %v", err)
	}
	// The marker cannot be written: the store fails instead of reusing
	// unverified snapshots.
	bad := t.TempDir()
	mustSymlink(t, filepath.Join(bad, "missing", "marker"), filepath.Join(bad, snapshotFormat))
	if _, err := newSnapshotStore(bad); err == nil {
		t.Fatal("unwritable format marker accepted")
	}
}

// The neutral attributes are rewritten before every command whose output
// the bot reads (a clone may have been writable in an exec container): a
// changed file, an info symlink pointing elsewhere are replaced; a .git that
// is no longer a directory fails every such command instead of running git
// with unknown attributes.
func TestCloneAttributesRewrittenAndFailClosed(t *testing.T) {
	r, mr := setupHostileMR(t)
	m := newManager(t, filepath.Dir(filepath.Dir(r.dir)))
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	c := co.(*Checkout)
	from := mr.DiffRefs.StartSHA
	info := filepath.Join(co.Dir(), ".git", "info")
	attrs := filepath.Join(info, "attributes")

	if err := os.WriteFile(attrs, []byte("*.go -diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if diff, err := co.Diff(ctx, from, nil); err != nil || !strings.Contains(diff, evilCode[0]) {
		t.Fatalf("tampered info/attributes honoured: %v\n%s", err, diff)
	}

	outside := t.TempDir()
	if err := os.RemoveAll(info); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, info)
	if files, err := co.ChangedFiles(ctx, from, nil); err != nil || len(files) != 6 {
		t.Fatalf("changed files %v %v", files, err)
	}
	if fi, err := os.Lstat(info); err != nil || !fi.IsDir() {
		t.Fatalf("info symlink not replaced: %v", err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("wrote through the info symlink: %v", ents)
	}

	// attributes is a directory: the rename fails, nothing runs.
	if err := os.Remove(attrs); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(attrs, "x"))
	if _, err := co.Diff(ctx, from, nil); err == nil {
		t.Fatal("diff ran without neutral attributes")
	}
	if ents, _ := os.ReadDir(info); len(ents) != 1 {
		t.Fatalf("temporary file left: %v", ents)
	}
	if err := os.RemoveAll(attrs); err != nil {
		t.Fatal(err)
	}

	gitDir := filepath.Join(co.Dir(), ".git")
	if err := os.Rename(gitDir, gitDir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitDir, []byte("gitdir: "+gitDir+".moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := co.Diff(ctx, from, nil); err == nil {
		t.Error("diff ran with .git not a directory")
	}
	if _, err := co.ChangedFiles(ctx, from, nil); err == nil {
		t.Error("changed files ran with .git not a directory")
	}
	if _, err := c.LinesChanged(ctx, from, "", "a.go", 1, 1); err == nil {
		t.Error("lines changed ran with .git not a directory")
	}
	if _, _, err := c.GrepWords(ctx, mr.DiffRefs.HeadSHA, []string{"evilA"}, nil, 0); err == nil {
		t.Error("grep ran with .git not a directory")
	}
	if _, _, err := c.ExportTree(ctx, mr.DiffRefs.HeadSHA, 0); err == nil {
		t.Error("export ran with .git not a directory")
	}
	if err := neutralizeAttributes(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing git dir accepted")
	}
}

// A mirror whose attributes cannot be neutralized is never used: the
// checkout, the cached tree and the snapshot export fail.
func TestMirrorAttributesFailClosed(t *testing.T) {
	r, mr := setupHostileMR(t)
	m := newManager(t, filepath.Dir(filepath.Dir(r.dir)))
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr) // the mirror has the commits
	if err != nil {
		t.Fatal(err)
	}
	co.Close()
	mirror := m.mirrorPath("group/proj")
	if err := os.Remove(filepath.Join(mirror, "info", "attributes")); err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(mirror, "info", "attributes", "x"))
	if _, err := m.Checkout(ctx, "group/proj", mr); err == nil {
		t.Error("checkout ignored the attributes failure")
	}
	if _, err := m.checkoutTree(ctx, mirror, mr.DiffRefs.HeadSHA, filepath.Join(t.TempDir(), "t"), 0, false); err == nil {
		t.Error("tree checked out without neutral attributes")
	}
	if _, err := m.exportSnapshot(ctx, mirror, mr.DiffRefs.HeadSHA, filepath.Join(t.TempDir(), "s"), 0); err == nil {
		t.Error("snapshot exported without neutral attributes")
	}
}
