package repo

// Checkouts from the cache of read-only trees (treecache.go).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/safefs"
)

// cachedCheckout returns a checkout whose working tree is the cached tree
// of the MR head (built when missing). The mirror is up to date.
func (m *Manager) cachedCheckout(ctx context.Context, mirror, projectPath string, mr *core.MergeRequest) (*Checkout, error) {
	head := mr.DiffRefs.HeadSHA
	dir, rel, err := m.trees.acquire(ctx, treeKey(projectPath, head, false), func(ctx context.Context, dir string) (int64, error) {
		return m.checkoutTree(ctx, mirror, head, dir, 0, false)
	})
	if err != nil {
		return nil, err
	}
	return &Checkout{dir: dir, git: m.git, log: m.log, mgr: m, mirror: mirror, project: projectPath,
		headSHA: head, releaseTree: rel}, nil
}

// cachedTree is ExportTree of a cached checkout: the shared read-only tree
// of rev (no .claude/ or .mcp.json), released by release or Close.
func (c *Checkout) cachedTree(ctx context.Context, rev string, maxBytes int64) (string, func(), error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", nil, errCheckoutClosed
	}
	c.mu.Unlock()
	// The cap is checked before the tree is written (and again for a cached
	// tree: the budget is the caller's).
	dir, rel, err := c.mgr.trees.acquire(ctx, treeKey(c.project, rev, true), func(ctx context.Context, dir string) (int64, error) {
		return c.mgr.checkoutTree(ctx, c.mirror, rev, dir, maxBytes, true)
	})
	if err == nil && maxBytes > 0 {
		if size := c.mgr.trees.sizeOf(treeKey(c.project, rev, true)); size > maxBytes {
			rel()
			err = ErrSnapshotTooLarge
		}
	}
	if err != nil {
		if errors.Is(err, ErrSnapshotTooLarge) {
			return "", nil, fmt.Errorf("repo: tree of %s: %w", shortRev(rev), core.ErrTreeTooLarge)
		}
		return "", nil, fmt.Errorf("repo: tree of %s: %w", shortRev(rev), err)
	}
	c.mu.Lock()
	if c.closed {
		// Close ran meanwhile and released the trees it knew of: this
		// reference is ours to give back.
		c.mu.Unlock()
		rel()
		return "", nil, errCheckoutClosed
	}
	if c.treeRel == nil {
		c.treeRel = map[int]func(){}
	}
	c.treeSeq++
	id := c.treeSeq
	c.treeRel[id] = rel
	c.mu.Unlock()
	release := func() {
		c.mu.Lock()
		r := c.treeRel[id]
		delete(c.treeRel, id)
		c.mu.Unlock()
		if r != nil {
			r()
		}
	}
	return dir, release, nil
}

// closeCached releases the trees of a cached checkout and removes its
// scratch directory, after a HistoryDir in progress (which then fails).
func (c *Checkout) closeCached() {
	c.historyMu.Lock()
	defer c.historyMu.Unlock()
	c.mu.Lock()
	rels := c.treeRel
	c.treeRel = nil
	scratch := c.scratch
	c.mu.Unlock()
	for _, r := range rels {
		r()
	}
	if c.releaseTree != nil {
		c.releaseTree()
	}
	if scratch != "" {
		if err := os.RemoveAll(scratch); err != nil {
			c.closeErr = err
			c.log.Warn("remove scratch dir failed", "dir", scratch, "err", err)
		}
	}
}

// scratchRoot creates (once) this job's writable directory under work/:
// never inside the shared tree.
func (c *Checkout) scratchRoot() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", errCheckoutClosed
	}
	if c.scratch != "" {
		return c.scratch, nil
	}
	name, err := randomName()
	if err != nil {
		return "", err
	}
	d := filepath.Join(c.mgr.workDir, name)
	if err := os.Mkdir(d, 0o755); err != nil {
		return "", fmt.Errorf("repo: %w", err)
	}
	c.scratch = d
	return d, nil
}

// ScratchDir returns a new empty writable directory name inside this job's
// scratch directory (outside the checkout's tree; removed by Close). The
// bot's deterministic checkers write there instead of into the tree
// (compat: .reviewbot-compat). Only cached checkouts implement it in
// effect; a clone returns "" (callers use the clone itself).
func (c *Checkout) ScratchDir(name string) (string, error) {
	if c.mgr == nil {
		return "", nil
	}
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("repo: bad scratch name %q", name)
	}
	root, err := c.scratchRoot()
	if err != nil {
		return "", err
	}
	return safefs.PrepareDir(root, name, true)
}

// HistoryDir returns a directory with a real .git directory holding the
// history of the MR, for tools that read git history (gitleaks): the clone
// itself, or, for a cached checkout, a clone of the mirror without a
// working tree (objects hard-linked, made once per job, inside the job's
// scratch directory; the tool may write its own files next to .git).
// Concurrent callers share the one clone; Close waits for a clone in
// progress.
func (c *Checkout) HistoryDir(ctx context.Context) (string, error) {
	if c.mgr == nil {
		return c.dir, nil
	}
	c.historyMu.Lock()
	defer c.historyMu.Unlock()
	root, err := c.scratchRoot()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	h := c.history
	c.mu.Unlock()
	if h != "" {
		return h, nil
	}
	dir := filepath.Join(root, "history")
	// --local without --shared: objects are hard-linked into a real .git
	// directory, no alternates file pointing back to the mirror (the
	// sandbox only sees this directory). Every object of the mirror comes
	// along, the MR's base and head included.
	lock := c.mgr.projectLock(c.project)
	lock.Lock()
	_, err = c.git.run(ctx, gitCmd{dir: root, args: []string{
		"clone", "--no-checkout", "--local", "--no-tags", "--quiet", "--", c.mirror, dir,
	}})
	lock.Unlock()
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("repo: history clone: %w", err)
	}
	_ = os.Remove(filepath.Join(dir, ".git", "objects", "info", "alternates"))
	// gitleaks reads the history with the sandbox's git: the MR's
	// .gitattributes must not hide code from it either.
	if err := neutralizeAttributes(filepath.Join(dir, ".git")); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if _, err := c.git.run(ctx, gitCmd{dir: dir, args: []string{"remote", "set-url", "origin", placeholderRemote(c.project)}}); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("repo: history clone: %w", err)
	}
	c.mu.Lock()
	closed := c.closed
	if !closed {
		c.history = dir
	}
	c.mu.Unlock()
	if closed {
		_ = os.RemoveAll(dir) // Close removes the scratch dir once we return
		return "", errCheckoutClosed
	}
	return dir, nil
}

// CacheStats reports the trees in the checkout cache and their size
// (0, 0 when the cache is off).
func (m *Manager) CacheStats() (trees int, bytes int64) {
	if m.trees == nil {
		return 0, 0
	}
	return m.trees.stats()
}

// Close stops the checkout cache janitor.
func (m *Manager) Close() {
	if m.trees != nil {
		m.trees.close()
	}
}
