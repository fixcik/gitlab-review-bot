package repo

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newGroupRemote creates a non-bare repo at <base>/<projectPath> with one
// commit on main and returns its dir.
func newGroupRemote(t *testing.T, base, projectPath string, files map[string]string) *remote {
	t.Helper()
	dir := filepath.Join(base, filepath.FromSlash(projectPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "init", "-q", "-b", "main")
	r := &remote{t: t, dir: dir}
	for p, c := range files {
		r.write(p, c)
	}
	r.commit("init")
	return r
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func newSnapManager(t *testing.T, base string) (*Manager, *fakeClock) {
	t.Helper()
	m := newManager(t, "file://"+base)
	clk := &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	m.snaps.now = clk.Now
	return m, clk
}

func snapshotDirs(t *testing.T, m *Manager) []string {
	t.Helper()
	entries, err := os.ReadDir(m.snaps.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Name() != snapshotFormat {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestSnapshotCreatedWithoutGitAndReused(t *testing.T) {
	base := t.TempDir()
	r := newGroupRemote(t, base, "billing/api", map[string]string{
		"client.go":      "package api\n\nfunc Charge() {}\n",
		"sub/handler.go": "package sub\n",
		".claude/x.json": "{}",
	})
	sha := gitT(t, r.dir, "rev-parse", "HEAD")
	m, clk := newSnapManager(t, base)
	ctx := context.Background()
	opt := SnapshotOptions{RefreshInterval: 10 * time.Minute}

	s1, err := m.Snapshot(ctx, SnapshotSpec{Path: "billing/api", DefaultBranch: "main"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Release()
	if s1.SHA != sha || s1.Branch != "main" || s1.Path != "billing/api" {
		t.Fatalf("snapshot %+v, want sha %s", s1, sha)
	}
	if !strings.HasSuffix(s1.Dir, "@"+sha) || !strings.HasPrefix(s1.Dir, m.snaps.dir) {
		t.Errorf("dir %s", s1.Dir)
	}
	if b, err := os.ReadFile(filepath.Join(s1.Dir, "sub", "handler.go")); err != nil || string(b) != "package sub\n" {
		t.Errorf("file content %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(s1.Dir, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git must not exist in a snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s1.Dir, ".claude")); !os.IsNotExist(err) {
		t.Errorf(".claude must be dropped: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(s1.Dir, "client.go")); fi.Mode().Perm()&0o222 != 0 {
		t.Errorf("snapshot files must be read-only: %v", fi.Mode())
	}
	if s1.Size == 0 {
		t.Error("size not measured")
	}

	// Same SHA after the refresh interval: the same directory is reused.
	clk.Add(11 * time.Minute)
	s2, err := m.Snapshot(ctx, SnapshotSpec{Path: "billing/api", DefaultBranch: "main"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Release()
	if s2.Dir != s1.Dir {
		t.Errorf("same sha must reuse the snapshot: %s vs %s", s2.Dir, s1.Dir)
	}
	if n := len(snapshotDirs(t, m)); n != 1 {
		t.Errorf("snapshots: %v", snapshotDirs(t, m))
	}

	// Default branch resolved from the remote when unknown.
	clk.Add(11 * time.Minute)
	s3, err := m.Snapshot(ctx, SnapshotSpec{Path: "billing/api"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	s3.Release()
	if s3.Branch != "main" || s3.SHA != sha {
		t.Errorf("ls-remote default branch: %+v", s3)
	}
}

func TestSnapshotNewSHACleansOldOnceReleased(t *testing.T) {
	base := t.TempDir()
	r := newGroupRemote(t, base, "g/p", map[string]string{"a.txt": "v1\n"})
	m, clk := newSnapManager(t, base)
	ctx := context.Background()
	opt := SnapshotOptions{RefreshInterval: time.Minute}
	spec := SnapshotSpec{Path: "g/p", DefaultBranch: "main"}

	old, err := m.Snapshot(ctx, spec, opt) // held by a "running review"
	if err != nil {
		t.Fatal(err)
	}
	r.write("a.txt", "v2\n")
	newSHA := r.commit("v2")
	clk.Add(2 * time.Minute)

	cur, err := m.Snapshot(ctx, spec, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Release()
	if cur.SHA != newSHA || cur.Dir == old.Dir {
		t.Fatalf("new sha must create a new snapshot: %+v", cur)
	}
	if b, _ := os.ReadFile(filepath.Join(cur.Dir, "a.txt")); string(b) != "v2\n" {
		t.Errorf("new content %q", b)
	}
	// The old one is still in use: kept.
	if _, err := os.Stat(old.Dir); err != nil {
		t.Fatalf("snapshot in use was removed: %v", err)
	}
	old.Release()
	old.Release() // idempotent
	if _, err := os.Stat(old.Dir); !os.IsNotExist(err) {
		t.Errorf("superseded snapshot not cleaned after release: %v", err)
	}
	if got := snapshotDirs(t, m); len(got) != 1 {
		t.Errorf("snapshots: %v", got)
	}
	// Releasing the current snapshot keeps it (it is reused next time).
	cur.Release()
	if _, err := os.Stat(cur.Dir); err != nil {
		t.Errorf("current snapshot removed: %v", err)
	}
}

func TestSnapshotRefreshInterval(t *testing.T) {
	base := t.TempDir()
	r := newGroupRemote(t, base, "g/p", map[string]string{"a.txt": "v1\n"})
	m, clk := newSnapManager(t, base)
	ctx := context.Background()
	opt := SnapshotOptions{RefreshInterval: 10 * time.Minute}
	spec := SnapshotSpec{Path: "g/p", DefaultBranch: "main"}

	s1, err := m.Snapshot(ctx, spec, opt)
	if err != nil {
		t.Fatal(err)
	}
	s1.Release()
	r.write("a.txt", "v2\n")
	newSHA := r.commit("v2")

	clk.Add(5 * time.Minute)
	s2, err := m.Snapshot(ctx, spec, opt)
	if err != nil {
		t.Fatal(err)
	}
	s2.Release()
	if s2.SHA != s1.SHA {
		t.Errorf("fetched before the refresh interval: %s", s2.SHA)
	}
	clk.Add(6 * time.Minute)
	s3, err := m.Snapshot(ctx, spec, opt)
	if err != nil {
		t.Fatal(err)
	}
	s3.Release()
	if s3.SHA != newSHA {
		t.Errorf("not refreshed after the interval: %s", s3.SHA)
	}
}

func TestSnapshotNeutralizesEscapingSymlinks(t *testing.T) {
	base := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newGroupRemote(t, base, "g/links", map[string]string{"real.txt": "ok\n"})
	if err := os.Symlink(secret, filepath.Join(r.dir, "abs-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../../../etc/passwd", filepath.Join(r.dir, "rel-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(r.dir, "in-tree")); err != nil {
		t.Fatal(err)
	}
	r.commit("links")
	m, _ := newSnapManager(t, base)
	s, err := m.Snapshot(context.Background(), SnapshotSpec{Path: "g/links", DefaultBranch: "main"}, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	for _, l := range []string{"abs-link", "rel-link"} {
		if _, err := os.Lstat(filepath.Join(s.Dir, l)); !os.IsNotExist(err) {
			t.Errorf("escaping symlink %s kept: %v", l, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(s.Dir, "in-tree")); err != nil || string(b) != "ok\n" {
		t.Errorf("in-tree symlink: %q %v", b, err)
	}
}

func TestSnapshotsSkipFailures(t *testing.T) {
	base := t.TempDir()
	newGroupRemote(t, base, "g/good", map[string]string{"a.txt": "a"})
	newGroupRemote(t, base, "g/other", map[string]string{"b.txt": "b"})
	m, _ := newSnapManager(t, base)
	specs := []SnapshotSpec{
		{Path: "g/good", DefaultBranch: "main"},
		{Path: "g/missing", DefaultBranch: "main"}, // no such repo
		{Path: "g/other", DefaultBranch: "no-such-branch"},
		{Path: "../evil", DefaultBranch: "main"},
		{Path: "g/other", DefaultBranch: "main"},
	}
	snaps := m.Snapshots(context.Background(), specs, SnapshotOptions{RefreshInterval: time.Minute})
	var got []string
	for _, s := range snaps {
		got = append(got, s.Path)
		s.Release()
	}
	if strings.Join(got, ",") != "g/good,g/other" {
		t.Fatalf("snapshots: %v", got)
	}
}

func TestSnapshotTooLarge(t *testing.T) {
	base := t.TempDir()
	newGroupRemote(t, base, "g/big", map[string]string{"big.txt": strings.Repeat("x", 4096)})
	m, _ := newSnapManager(t, base)
	_, err := m.Snapshot(context.Background(), SnapshotSpec{Path: "g/big", DefaultBranch: "main"}, SnapshotOptions{MaxBytes: 1000})
	if !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("err = %v", err)
	}
	for _, d := range snapshotDirs(t, m) {
		t.Errorf("leftover %s", d)
	}
	// Cached error within the refresh interval.
	_, err = m.Snapshot(context.Background(), SnapshotSpec{Path: "g/big", DefaultBranch: "main"}, SnapshotOptions{MaxBytes: 1000, RefreshInterval: time.Hour})
	if !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestSnapshotRefreshFailureKeepsPrevious(t *testing.T) {
	base := t.TempDir()
	r := newGroupRemote(t, base, "g/p", map[string]string{"a.txt": "a"})
	m, clk := newSnapManager(t, base)
	opt := SnapshotOptions{RefreshInterval: time.Minute}
	s1, err := m.Snapshot(context.Background(), SnapshotSpec{Path: "g/p", DefaultBranch: "main"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	s1.Release()
	if err := os.RemoveAll(r.dir); err != nil { // GitLab unreachable
		t.Fatal(err)
	}
	clk.Add(2 * time.Minute)
	s2, err := m.Snapshot(context.Background(), SnapshotSpec{Path: "g/p", DefaultBranch: "main"}, opt)
	if err != nil {
		t.Fatalf("previous snapshot must be served: %v", err)
	}
	s2.Release()
	if s2.Dir != s1.Dir {
		t.Errorf("dir %s vs %s", s2.Dir, s1.Dir)
	}
}

func TestSnapshotConcurrent(t *testing.T) {
	base := t.TempDir()
	r := newGroupRemote(t, base, "g/p", map[string]string{"a.txt": "v0"})
	newGroupRemote(t, base, "g/q", map[string]string{"b.txt": "q"})
	m, clk := newSnapManager(t, base)
	ctx := context.Background()
	opt := SnapshotOptions{RefreshInterval: time.Minute}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for round := 0; round < 3; round++ {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				snaps := m.Snapshots(ctx, []SnapshotSpec{{Path: "g/p", DefaultBranch: "main"}, {Path: "g/q", DefaultBranch: "main"}}, opt)
				if len(snaps) != 2 {
					errs <- errors.New("missing snapshots")
				}
				for _, s := range snaps {
					if _, err := os.Stat(filepath.Join(s.Dir)); err != nil {
						errs <- err
					}
					s.Release()
				}
			}()
		}
		wg.Wait()
		r.write("a.txt", "v"+string(rune('1'+round)))
		r.commit("next")
		clk.Add(2 * time.Minute)
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Everything released: only the current snapshot of each repo remains.
	if got := snapshotDirs(t, m); len(got) != 2 {
		t.Errorf("snapshots left: %v", got)
	}
}

func TestExtractTarRejectsEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "/abs", "a/../../evil"} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("x"))
		_ = tw.Close()
		if _, err := extractTar(&buf, t.TempDir(), 0); err == nil {
			t.Errorf("%q: expected error", name)
		}
	}
	// A symlink to a directory, then a file "through" it: the link is created
	// last, so the file lands in a real directory inside root.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	outside := t.TempDir()
	_ = tw.WriteHeader(&tar.Header{Name: "d", Linkname: outside, Typeflag: tar.TypeSymlink})
	_ = tw.WriteHeader(&tar.Header{Name: "d/f", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	root := t.TempDir()
	if _, err := extractTar(&buf, root, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "f")); !os.IsNotExist(err) {
		t.Error("file written through a symlink")
	}
}

func TestParseSymrefHeadAndValidProjectPath(t *testing.T) {
	b, err := parseSymrefHead("ref: refs/heads/trunk\tHEAD\nabc\tHEAD\n")
	if err != nil || b != "trunk" {
		t.Errorf("got %q %v", b, err)
	}
	if _, err := parseSymrefHead("abc\tHEAD\n"); err == nil {
		t.Error("expected error without symref")
	}
	for p, ok := range map[string]bool{
		"g/p": true, "group/sub/my-proj.v2": true, "../x": false, "g/../p": false,
		"g/p.git": false, "g:p/x": false, "g/-p": false, "": false, "/g/p": false, "g p/x": false,
	} {
		if ValidProjectPath(p) != ok {
			t.Errorf("ValidProjectPath(%q) != %v", p, ok)
		}
	}
}
