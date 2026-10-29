package repo

// Checkout cache: read-only working trees per (project, commit), shared by
// the jobs that need the same commit and kept after use.
//
// A tree is the full set of files of one commit (not a diff), checked out
// from the project's bare mirror into trees/<project>@<sha>[-base] with a
// throwaway index (`git --git-dir=<mirror> --work-tree=<tree> read-tree -u`):
// like a `git worktree`, but without the .git link file and without an
// entry under the mirror's worktrees/ (nothing to prune, nothing a mirror
// fetch or gc can trip over). History and objects stay in the mirror only;
// the extra disk per tree is just its working files. Git commands of a
// checkout run against the mirror with the head SHA.
//
// Trees are immutable while cached: files 0444, directories 0555, out-of-tree
// symlinks removed; the sandbox mounts them read-only; the bot writes its
// own files (lint / compat scratch, the gitleaks history clone, the MCP
// config) elsewhere.
//
// Lifetime: acquire counts references; a tree without references stays for
// the TTL after its last release (a re-review of the same head, or a review
// of another MR on the same merge base, reuses it) and is evicted earlier,
// least recently used first, when the cache is over its disk budget. A
// janitor goroutine sweeps periodically; the cache directory is emptied at
// startup (the index lives in memory).

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// treeCache is the cache of read-only trees.
type treeCache struct {
	dir      string
	ttl      time.Duration
	maxBytes int64
	log      *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]*treeEntry

	stop     chan struct{}
	stopOnce sync.Once
}

type treeEntry struct {
	key, dir string
	ready    chan struct{} // closed when built (err set on failure)
	err      error
	size     int64
	refs     int
	lastUsed time.Time
}

// newTreeCache empties dir and returns the cache; with janitor a goroutine
// sweeps every interval until close.
func newTreeCache(dir string, ttl time.Duration, maxBytes int64, log *slog.Logger, janitor bool) (*treeCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	c := &treeCache{dir: dir, ttl: ttl, maxBytes: maxBytes, log: log, now: time.Now,
		entries: map[string]*treeEntry{}, stop: make(chan struct{})}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			// Trees of the previous process, half-built trees, leftover MCP
			// config dirs of interrupted runs.
			removeTree(filepath.Join(dir, e.Name()), log)
		}
	}
	if janitor {
		go c.janitor(janitorInterval(ttl))
	}
	return c, nil
}

// janitorInterval: a quarter of the TTL, between 30s and 5m.
func janitorInterval(ttl time.Duration) time.Duration {
	return min(max(ttl/4, 30*time.Second), 5*time.Minute)
}

func (c *treeCache) janitor(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.sweep()
		}
	}
}

func (c *treeCache) close() {
	c.stopOnce.Do(func() { close(c.stop) })
}

// acquire returns the tree of key, building it with build (into a fresh
// directory it fills and returns the size of) when it is not cached. Callers
// of the same key share one build; a failed build is not cached. release
// must be called exactly once.
func (c *treeCache) acquire(ctx context.Context, key string, build func(ctx context.Context, dir string) (int64, error)) (string, func(), error) {
	for {
		c.mu.Lock()
		e, ok := c.entries[key]
		if !ok {
			e = &treeEntry{key: key, dir: filepath.Join(c.dir, key), ready: make(chan struct{}), refs: 1}
			c.entries[key] = e
			c.mu.Unlock()
			c.build(ctx, e, build)
			if e.err != nil {
				return "", nil, e.err
			}
			return e.dir, c.releaser(e), nil
		}
		e.refs++
		c.mu.Unlock()
		select {
		case <-e.ready:
		case <-ctx.Done():
			c.unref(e)
			return "", nil, ctx.Err()
		}
		if e.err == nil {
			return e.dir, c.releaser(e), nil
		}
		c.unref(e)
		// The build we waited for failed (maybe its own job was
		// cancelled): it is gone from the map, try ourselves.
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
	}
}

// build fills e and wakes its waiters; a failed build leaves the map.
func (c *treeCache) build(ctx context.Context, e *treeEntry, build func(ctx context.Context, dir string) (int64, error)) {
	size, err := c.buildTree(ctx, e.dir, build)
	c.mu.Lock()
	if err != nil {
		e.err = err
		delete(c.entries, e.key)
	} else {
		e.size, e.lastUsed = size, c.now()
	}
	c.mu.Unlock()
	close(e.ready)
	if err == nil {
		c.sweep() // the new tree may put the cache over its budget
	}
}

// buildTree builds into a fresh temporary directory, makes it read-only and
// moves it to dir. A failed build leaves nothing behind.
func (c *treeCache) buildTree(ctx context.Context, dir string, build func(ctx context.Context, dir string) (int64, error)) (int64, error) {
	name, err := randomName()
	if err != nil {
		return 0, err
	}
	tmp := filepath.Join(c.dir, ".tmp-"+name)
	size, err := build(ctx, tmp)
	if err == nil {
		makeReadOnly(tmp)
		err = os.Rename(tmp, dir)
	}
	if err != nil {
		removeTree(tmp, c.log)
		return 0, err
	}
	return size, nil
}

func (c *treeCache) releaser(e *treeEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			c.unref(e)
			c.sweep()
		})
	}
}

func (c *treeCache) unref(e *treeEntry) {
	c.mu.Lock()
	e.refs--
	e.lastUsed = c.now()
	c.mu.Unlock()
}

// sweep evicts unused trees older than the TTL, then unused trees least
// recently used first while the cache is over its budget.
func (c *treeCache) sweep() {
	now := c.now()
	var drop []*treeEntry
	c.mu.Lock()
	var total int64
	var idle []*treeEntry
	for _, e := range c.entries {
		if !isClosed(e.ready) || e.err != nil {
			continue
		}
		if e.refs <= 0 && now.Sub(e.lastUsed) >= c.ttl {
			drop = append(drop, e)
			continue
		}
		total += e.size
		if e.refs <= 0 {
			idle = append(idle, e)
		}
	}
	if c.maxBytes > 0 && total > c.maxBytes {
		sort.Slice(idle, func(i, j int) bool { return idle[i].lastUsed.Before(idle[j].lastUsed) })
		for _, e := range idle {
			if total <= c.maxBytes {
				break
			}
			drop = append(drop, e)
			total -= e.size
		}
		if total > c.maxBytes {
			c.log.Warn("checkout cache over its disk budget: every tree is in use", "bytes", total, "max_bytes", c.maxBytes)
		}
	}
	// Moved aside under the lock: a new tree of the same key may be built
	// right away and must not meet the old directory.
	trash := make([]string, 0, len(drop))
	for _, e := range drop {
		delete(c.entries, e.key)
		t := e.dir
		if name, err := randomName(); err == nil {
			evicted := filepath.Join(c.dir, ".evicted-"+name)
			if os.Rename(e.dir, evicted) == nil {
				t = evicted
			}
		}
		trash = append(trash, t)
	}
	c.mu.Unlock()
	for i, e := range drop {
		c.log.Info("checkout cache: tree evicted", "tree", e.key, "bytes", e.size, "idle", now.Sub(e.lastUsed).Round(time.Second))
		removeTree(trash[i], c.log)
	}
}

// sizeOf is the size of a built tree (0 = not cached).
func (c *treeCache) sizeOf(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok && isClosed(e.ready) && e.err == nil {
		return e.size
	}
	return 0
}

// stats reports the cached trees and their size (tests, logs).
func (c *treeCache) stats() (trees int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if isClosed(e.ready) && e.err == nil {
			trees++
			bytes += e.size
		}
	}
	return trees, bytes
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// makeReadOnly makes every regular file 0444 (0555 if executable) and every
// directory 0555. Symlinks are left alone (chmod would follow them).
func makeReadOnly(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		switch {
		case d.IsDir():
			dirs = append(dirs, p)
		case d.Type().IsRegular():
			mode := os.FileMode(0o444)
			if fi, err := d.Info(); err == nil && fi.Mode()&0o111 != 0 {
				mode = 0o555
			}
			_ = os.Chmod(p, mode)
		}
		return nil
	})
	// Directories last, deepest first: a 0555 directory cannot be changed.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chmod(dirs[i], 0o555)
	}
}

// removeTree deletes a (read-only) tree: directories are made writable
// first, without following symlinks.
func removeTree(root string, log *slog.Logger) {
	if _, err := os.Lstat(root); err != nil {
		return
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	if err := os.RemoveAll(root); err != nil && log != nil {
		log.Warn("checkout cache: remove tree failed", "dir", root, "err", err)
	}
}

// treeKey is the cache key of a commit of a project; base trees (the /base
// export: no .claude/ or .mcp.json) are a separate variant.
func treeKey(projectPath, sha string, base bool) string {
	k := sanitize(projectPath) + "@" + strings.ToLower(sha)
	if base {
		k += "-base"
	}
	return k
}

// treeSize sums the blob sizes of the tree of sha (`git ls-tree -r -l`).
func (m *Manager) treeSize(ctx context.Context, mirror, sha string) (int64, error) {
	out, err := m.git.run(ctx, gitCmd{dir: mirror, args: []string{"ls-tree", "-r", "-l", "-z", "--full-tree", sha}})
	if err != nil {
		return 0, fmt.Errorf("repo: ls-tree: %w", err)
	}
	var total int64
	for _, rec := range strings.Split(string(out), "\x00") {
		// "<mode> <type> <object> <size>\t<path>"
		meta, _, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 4 || f[1] != "blob" {
			continue
		}
		var n int64
		if _, err := fmt.Sscan(f[3], &n); err == nil {
			total += n
		}
	}
	return total, nil
}

// checkoutTree writes the files of sha from the mirror into dir (created)
// with a throwaway index: no .git in dir, nothing registered in the mirror.
// maxBytes > 0 caps the size (ErrSnapshotTooLarge, checked before writing).
// base drops .claude/ and .mcp.json at the root (like an export).
func (m *Manager) checkoutTree(ctx context.Context, mirror, sha, dir string, maxBytes int64, base bool) (int64, error) {
	size, err := m.treeSize(ctx, mirror, sha)
	if err != nil {
		return 0, err
	}
	if maxBytes > 0 && size > maxBytes {
		return 0, ErrSnapshotTooLarge
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("repo: %w", err)
	}
	idx := dir + ".index"
	defer os.Remove(idx)
	if err := neutralizeAttributes(mirror); err != nil {
		return 0, err
	}
	if _, err := m.git.run(ctx, gitCmd{dir: mirror, env: []string{"GIT_INDEX_FILE=" + idx}, timeout: exportTimeout,
		args: []string{"--git-dir=" + mirror, "--work-tree=" + dir, "read-tree", "-u", "--reset", sha}}); err != nil {
		return 0, fmt.Errorf("repo: check out %s: %w", shortRev(sha), err)
	}
	if base {
		_ = os.RemoveAll(filepath.Join(dir, ".claude"))
		_ = os.Remove(filepath.Join(dir, ".mcp.json"))
	}
	// A tracked file named .git (or a directory) would make git and tools
	// treat the tree as a repository.
	_ = os.RemoveAll(filepath.Join(dir, ".git"))
	if n := neutralizeSymlinks(dir, m.log); n > 0 {
		m.log.Warn("neutralized out-of-tree symlinks in cached tree", "sha", shortRev(sha), "count", n)
	}
	return size, nil
}
