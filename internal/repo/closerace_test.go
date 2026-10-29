package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// gatedGit makes m run the real git, except that a command whose argv
// contains word first records itself in gate/started-<pid> and then, with
// block, waits for gate/go.
func gatedGit(t *testing.T, m *Manager, word string, block bool) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gate := t.TempDir()
	wait := ":"
	if block {
		wait = `while [ ! -f "` + gate + `/go" ]; do sleep 0.01; done`
	}
	m.git.bin = fakeGit(t, `case "$*" in *`+word+`*) touch "`+gate+`/started-$$"; `+wait+`;; esac
exec "`+real+`" "$@"
`)
	return gate
}

// started waits until n gated commands have started.
func started(t *testing.T, gate string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m, _ := filepath.Glob(filepath.Join(gate, "started-*")); len(m) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gated git did not start")
}

func open(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(gate, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A cached ExportTree that overlaps Close fails and gives its tree
// reference back (a leaked reference would pin the tree in the cache).
func TestCachedExportTreeOverlappingClose(t *testing.T) {
	r, mr := setupMR(t)
	m := newCachedManager(t, filepath.Dir(filepath.Dir(r.dir)), time.Hour, 0)
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	c := co.(*Checkout)
	gate := gatedGit(t, m, "read-tree", true)
	type result struct {
		rel func()
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, rel, err := c.ExportTree(ctx, mr.DiffRefs.BaseSHA, 0)
		done <- result{rel, err}
	}()
	started(t, gate, 1)
	if err := co.Close(); err != nil {
		t.Fatal(err)
	}
	open(t, gate)
	res := <-done
	if res.err == nil {
		res.rel()
		t.Fatal("ExportTree overlapping Close succeeded")
	}
	m.trees.mu.Lock()
	e := m.trees.entries[treeKey("group/proj", mr.DiffRefs.BaseSHA, true)]
	refs := -1
	if e != nil {
		refs = e.refs
	}
	m.trees.mu.Unlock()
	if refs != 0 {
		t.Fatalf("base tree refs after Close = %d, want 0", refs)
	}
}

// Concurrent HistoryDir calls share one history clone.
func TestHistoryDirConcurrentCallers(t *testing.T) {
	r, mr := setupMR(t)
	m := newCachedManager(t, filepath.Dir(filepath.Dir(r.dir)), time.Hour, 0)
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	c := co.(*Checkout)
	gate := gatedGit(t, m, "clone", false)
	var wg sync.WaitGroup
	dirs := make([]string, 2)
	errs := make([]error, 2)
	for i := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dirs[i], errs[i] = c.HistoryDir(ctx)
		}()
	}
	wg.Wait()
	for i := range dirs {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
	}
	if dirs[0] != dirs[1] {
		t.Fatalf("two history dirs: %v", dirs)
	}
	if fi, err := os.Stat(filepath.Join(dirs[0], ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("history clone gone: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(gate, "started-*")); len(m) != 1 {
		t.Fatalf("%d history clones, want 1", len(m))
	}
}

// A HistoryDir in progress when Close is called fails and leaves nothing:
// Close waits for it rather than removing the scratch dir under it.
func TestHistoryDirOverlappingClose(t *testing.T) {
	r, mr := setupMR(t)
	m := newCachedManager(t, filepath.Dir(filepath.Dir(r.dir)), time.Hour, 0)
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	c := co.(*Checkout)
	gate := gatedGit(t, m, "clone", true)
	done := make(chan error, 1)
	go func() {
		_, err := c.HistoryDir(ctx)
		done <- err
	}()
	started(t, gate, 1)
	closed := make(chan struct{})
	go func() {
		co.Close()
		close(closed)
	}()
	// Let Close run into the clone in progress, then let the clone finish.
	time.Sleep(50 * time.Millisecond)
	open(t, gate)
	if err := <-done; err == nil {
		t.Fatal("HistoryDir overlapping Close succeeded")
	}
	<-closed
	if ents, _ := os.ReadDir(m.workDir); len(ents) != 0 {
		t.Fatalf("left in work dir after Close: %v", ents)
	}
}
