package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// pathspecChunk is how many paths go into one git diff invocation.
const pathspecChunk = 200

// Checkout is one disposable working tree. It implements core.Checkout.
type Checkout struct {
	dir string
	git *gitRunner
	log *slog.Logger

	closeOnce sync.Once
	closeErr  error

	// exports are the ExportTree directories not released yet (removed by
	// Close at the latest).
	mu      sync.Mutex
	exports map[string]bool
	closed  bool

	// Cached mode (treecache.go; mgr != nil): dir is a shared read-only
	// tree of the head; git runs in the mirror with the head SHA; exports
	// are cached trees too (treeRel: their releases). scratch is this job's
	// own writable directory (scratch.go), removed by Close.
	mgr         *Manager
	mirror      string
	project     string
	headSHA     string
	releaseTree func()
	treeRel     map[int]func()
	treeSeq     int
	scratch     string
	history     string
	// historyMu serializes HistoryDir (and Close against it).
	historyMu sync.Mutex
}

// errCheckoutClosed: the checkout was closed (maybe while the call ran).
var errCheckoutClosed = errors.New("repo: checkout closed")

// repoDir is where git runs: the clone, or the mirror for a cached tree.
func (c *Checkout) repoDir() string {
	if c.mgr != nil {
		return c.mirror
	}
	return c.dir
}

// head names the reviewed commit for git: HEAD of the clone, or the head
// SHA (a cached tree has no HEAD of its own).
func (c *Checkout) head() string {
	if c.mgr != nil {
		return c.headSHA
	}
	return "HEAD"
}

var (
	_ core.Checkout     = (*Checkout)(nil)
	_ core.TreeExporter = (*Checkout)(nil)
)

// Dir implements core.Checkout.
func (c *Checkout) Dir() string { return c.dir }

// ChangedFiles implements core.Checkout. Renames are reported by new path.
func (c *Checkout) ChangedFiles(ctx context.Context, from string, ignore []string) ([]string, error) {
	if !validRev(from) {
		return nil, fmt.Errorf("repo: invalid revision %q", from)
	}
	if err := c.neutralAttrs(); err != nil {
		return nil, err
	}
	out, err := c.git.run(ctx, gitCmd{dir: c.repoDir(), args: []string{
		"-c", "core.quotePath=false",
		"diff", "--name-only", "-z", "--no-ext-diff", "--no-textconv", "-M", from, c.head(),
	}})
	if err != nil {
		return nil, fmt.Errorf("repo: changed files: %w", err)
	}
	var files []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) == 0 {
			continue
		}
		name := string(p)
		if ignored(name, ignore) {
			continue
		}
		files = append(files, name)
	}
	return files, nil
}

// Diff implements core.Checkout.
func (c *Checkout) Diff(ctx context.Context, from string, ignore []string) (string, error) {
	if !validRev(from) {
		return "", fmt.Errorf("repo: invalid revision %q", from)
	}
	if err := c.neutralAttrs(); err != nil {
		return "", err
	}
	base := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "-M", from, c.head()}
	if len(ignore) == 0 {
		out, err := c.git.run(ctx, gitCmd{dir: c.repoDir(), args: base})
		if err != nil {
			return "", fmt.Errorf("repo: diff: %w", err)
		}
		return string(out), nil
	}

	// With renames, a file is shown under its new name; pathspecs must cover
	// both sides for -M to pair them, so collect old and new names.
	out, err := c.git.run(ctx, gitCmd{dir: c.repoDir(), args: []string{
		"diff", "--name-status", "-z", "--no-ext-diff", "--no-textconv", "-M", from, c.head(),
	}})
	if err != nil {
		return "", fmt.Errorf("repo: diff: %w", err)
	}
	groups := keptPaths(out, ignore)
	if len(groups) == 0 {
		return "", nil
	}
	var sb strings.Builder
	for len(groups) > 0 {
		// Fill a chunk without splitting a rename pair.
		args := append(append([]string{}, base...), "--")
		n := 0
		for len(groups) > 0 && (n == 0 || n+len(groups[0]) <= pathspecChunk) {
			for _, p := range groups[0] {
				// Literal pathspecs: file names with glob chars must not expand.
				args = append(args, ":(literal)"+p)
			}
			n += len(groups[0])
			groups = groups[1:]
		}
		out, err := c.git.run(ctx, gitCmd{dir: c.repoDir(), args: args})
		if err != nil {
			return "", fmt.Errorf("repo: diff: %w", err)
		}
		sb.Write(out)
	}
	return sb.String(), nil
}

// keptPaths parses `git diff --name-status -z` output and returns, per kept
// entry, its paths (old and new for renames/copies, so git can still pair
// them). An entry is kept when its new path is not ignored.
func keptPaths(out []byte, ignore []string) [][]string {
	fields := bytes.Split(out, []byte{0})
	var groups [][]string
	for i := 0; i < len(fields); {
		status := string(fields[i])
		if status == "" {
			i++
			continue
		}
		if status[0] == 'R' || status[0] == 'C' {
			if i+2 >= len(fields) {
				break
			}
			oldP, newP := string(fields[i+1]), string(fields[i+2])
			i += 3
			if !ignored(newP, ignore) {
				groups = append(groups, []string{oldP, newP})
			}
			continue
		}
		if i+1 >= len(fields) {
			break
		}
		p := string(fields[i+1])
		i += 2
		if !ignored(p, ignore) {
			groups = append(groups, []string{p})
		}
	}
	return groups
}

// ignored reports whether path matches any doublestar glob.
func ignored(path string, globs []string) bool {
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if ok, err := doublestar.Match(g, path); err == nil && ok {
			return true
		}
	}
	return false
}

// IsAncestor implements core.Checkout.
func (c *Checkout) IsAncestor(ctx context.Context, commit string) (bool, error) {
	if !validRev(commit) {
		return false, nil
	}
	if !c.git.hasCommit(ctx, c.repoDir(), commit) {
		return false, nil
	}
	_, err := c.git.run(ctx, gitCmd{dir: c.repoDir(), args: []string{"merge-base", "--is-ancestor", commit, c.head()}})
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("repo: is-ancestor: %w", err)
}

// ShowFile implements core.Checkout. It reads the raw blob with
// `git cat-file blob`, which never runs textconv or filter drivers.
func (c *Checkout) ShowFile(ctx context.Context, rev, path string) ([]byte, error) {
	if !validRev(rev) {
		return nil, fmt.Errorf("repo: invalid revision %q", rev)
	}
	if path == "" || strings.ContainsAny(path, "\x00\n") {
		return nil, fmt.Errorf("repo: invalid path %q", path)
	}
	if _, err := c.git.run(ctx, gitCmd{dir: c.repoDir(), args: []string{"cat-file", "-e", rev + ":" + path}}); err != nil {
		if code := exitCode(err); code == 1 || code == 128 {
			return nil, fmt.Errorf("repo: %s:%s: %w", shortRev(rev), path, core.ErrNotFound)
		}
		return nil, fmt.Errorf("repo: show file: %w", err)
	}
	out, err := c.git.run(ctx, gitCmd{dir: c.repoDir(), args: []string{"cat-file", "blob", rev + ":" + path}})
	if err != nil {
		return nil, fmt.Errorf("repo: show file: %w", err)
	}
	return out, nil
}

// ListFiles implements core.Checkout.
func (c *Checkout) ListFiles(ctx context.Context, rev string) ([]string, error) {
	if !validRev(rev) {
		return nil, fmt.Errorf("repo: invalid revision %q", rev)
	}
	out, err := c.git.run(ctx, gitCmd{dir: c.repoDir(), args: []string{"ls-tree", "-r", "-z", "--name-only", "--full-tree", rev}})
	if err != nil {
		return nil, fmt.Errorf("repo: list files: %w", err)
	}
	var files []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			files = append(files, string(p))
		}
	}
	return files, nil
}

var _ core.WordGrepper = (*Checkout)(nil)

// grepTimeout bounds one GrepWords call.
const grepTimeout = 60 * time.Second

// grepWordRe: what GrepWords accepts as a word (passed with -F -e, never as
// an option or a regular expression).
var grepWordRe = regexp.MustCompile(`^[A-Za-z0-9_$]{1,128}$`)

// GrepWords implements core.WordGrepper with a read-only `git grep -w -F`
// over the tree of rev (blobs are read raw: no textconv, no filters, binary
// files skipped). It never touches the working tree or the index.
func (c *Checkout) GrepWords(ctx context.Context, rev string, words, pathspecs []string, maxBytes int) ([]core.GrepMatch, bool, error) {
	if !validRev(rev) {
		return nil, false, fmt.Errorf("repo: invalid revision %q", rev)
	}
	if len(words) == 0 {
		return nil, false, nil
	}
	args := []string{"grep", "-n", "-w", "-F", "-I", "--null", "--no-color", "--full-name", "--no-textconv"}
	for _, w := range words {
		if !grepWordRe.MatchString(w) {
			return nil, false, fmt.Errorf("repo: invalid grep word %q", w)
		}
		args = append(args, "-e", w)
	}
	args = append(args, rev, "--")
	for _, p := range pathspecs {
		if p == "" || strings.ContainsAny(p, "\x00\n") {
			return nil, false, fmt.Errorf("repo: invalid pathspec %q", p)
		}
		args = append(args, p)
	}
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}
	if err := c.neutralAttrs(); err != nil {
		return nil, false, err
	}
	out, truncated, err := c.git.runCapped(ctx, gitCmd{dir: c.repoDir(), args: args, timeout: grepTimeout}, maxBytes)
	if err != nil && exitCode(err) != 1 { // 1: nothing matched
		return nil, false, fmt.Errorf("repo: grep: %w", err)
	}
	return parseGrep(out, rev, truncated), truncated, nil
}

// parseGrep reads `git grep -n --null <rev>` output: "rev:path\0line\0text".
// A cut-off last line (truncated output) is dropped.
func parseGrep(out []byte, rev string, truncated bool) []core.GrepMatch {
	lines := bytes.Split(out, []byte{'\n'})
	if truncated && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	var ms []core.GrepMatch
	prefix := []byte(rev + ":")
	for _, l := range lines {
		l = bytes.TrimPrefix(l, prefix)
		parts := bytes.SplitN(l, []byte{0}, 3)
		if len(parts) != 3 {
			continue
		}
		n, err := strconv.Atoi(string(parts[1]))
		if err != nil || len(parts[0]) == 0 {
			continue
		}
		ms = append(ms, core.GrepMatch{Path: string(parts[0]), Line: n, Text: string(parts[2])})
	}
	return ms
}

func shortRev(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// exportTimeout bounds one ExportTree (git archive of the whole tree).
const exportTimeout = 5 * time.Minute

// ExportTree implements core.TreeExporter: `git archive` of rev from the
// clone, unpacked like a context snapshot (extractTar: regular files 0444,
// no .git / .claude / .mcp.json, symlinks created last and out-of-tree ones
// removed) into a sibling of the clone (<work>/<clone>-tree-<random>). A
// sibling, not a subdirectory: the clone is mounted read-write, the export
// read-only; and it lives under the same host path as the clone, so the
// docker daemon can mount it when the bot itself runs in a container.
func (c *Checkout) ExportTree(ctx context.Context, rev string, maxBytes int64) (string, func(), error) {
	if !validRev(rev) {
		return "", nil, fmt.Errorf("repo: invalid revision %q", rev)
	}
	if c.mgr != nil {
		return c.cachedTree(ctx, rev, maxBytes)
	}
	name, err := randomName()
	if err != nil {
		return "", nil, err
	}
	dir := c.dir + "-tree-" + name
	// Created before it is registered: a Close in between either sees it
	// (and removes it) or has already marked the checkout closed.
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("repo: %w", err)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.removeExport(dir)
		return "", nil, errCheckoutClosed
	}
	if c.exports == nil {
		c.exports = map[string]bool{}
	}
	c.exports[dir] = true
	c.mu.Unlock()
	release := func() {
		c.mu.Lock()
		owned := c.exports[dir]
		delete(c.exports, dir)
		c.mu.Unlock()
		if owned {
			c.removeExport(dir)
		}
	}
	if err := c.neutralAttrs(); err != nil {
		release()
		return "", nil, err
	}
	err = c.git.stream(ctx, gitCmd{dir: c.dir, timeout: exportTimeout, args: []string{"archive", "--format=tar", rev}}, func(r io.Reader) error {
		_, xerr := extractTar(r, dir, maxBytes)
		return xerr
	})
	if err != nil {
		release()
		if errors.Is(err, ErrSnapshotTooLarge) {
			return "", nil, fmt.Errorf("repo: tree of %s: %w", shortRev(rev), core.ErrTreeTooLarge)
		}
		return "", nil, fmt.Errorf("repo: export tree of %s: %w", shortRev(rev), err)
	}
	if n := neutralizeSymlinks(dir, c.log); n > 0 {
		c.log.Warn("neutralized out-of-tree symlinks in exported tree", "rev", rev, "count", n)
	}
	return dir, release, nil
}

// removeExport deletes an exported tree. Its directories are 0755, so the
// read-only files inside can be removed.
func (c *Checkout) removeExport(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		c.log.Warn("remove exported tree failed", "dir", dir, "err", err)
	}
}

// Close implements core.Checkout. It is safe to call more than once.
func (c *Checkout) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		exports := c.exports
		c.exports = nil
		c.mu.Unlock()
		for d := range exports {
			c.removeExport(d)
		}
		if c.mgr != nil {
			c.closeCached()
			return
		}
		c.closeErr = os.RemoveAll(c.dir)
		if c.closeErr != nil {
			c.log.Warn("remove work dir failed", "dir", c.dir, "err", c.closeErr)
		}
	})
	return c.closeErr
}
