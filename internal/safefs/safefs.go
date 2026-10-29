// Package safefs creates the bot's own paths inside an MR clone without
// following anything the MR (or a checker run with the clone mounted)
// planted there.
//
// The clone is attacker-controlled: a committed symlink at
// .reviewbot-lint, .reviewbot-compat or .git/info must not
// make the bot create directories or write files outside the clone. Every
// function here works below a root directory (the clone): the bot's own
// top-level directories are created with Lstat + Mkdir (never MkdirAll
// through a link) and verified with EvalSymlinks, and files are written via
// os.Root, which refuses to resolve a path outside the root.
package safefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// PrepareDir makes root/name a real directory inside root and returns its
// absolute path. name must be a single path component (".reviewbot-lint").
//
// Whatever exists at root/name that is not a plain directory (a symlink, a
// file) is removed first, without following it. With fresh, an existing
// directory is removed too (os.RemoveAll does not follow symlinks inside
// it) so the result is empty; without fresh an existing real directory is
// kept. root itself must be a real directory, not a symlink.
func PrepareDir(root, name string, fresh bool) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("safefs: bad directory name %q", name)
	}
	realRoot, err := realDir(root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", fmt.Errorf("safefs: %w", err)
	case !fi.IsDir(): // symlink (to anything), regular file, device, ...
		if err := os.Remove(dir); err != nil {
			return "", fmt.Errorf("safefs: remove %s: %w", name, err)
		}
	case fresh:
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("safefs: clean %s: %w", name, err)
		}
	}
	// os.Mkdir does not follow a final symlink: if one appeared in between,
	// it fails with EEXIST and the check below rejects it.
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("safefs: create %s: %w", name, err)
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("safefs: %s is not a directory inside the clone", name)
	}
	if err := within(realRoot, dir, filepath.Join(realRoot, name)); err != nil {
		return "", err
	}
	return dir, nil
}

// WriteFile writes data to rel (a slash path below root), creating missing
// parent directories. Nothing outside root is ever created or written: every
// component is resolved by os.Root, which rejects symlinks that leave root.
func WriteFile(root, rel string, data []byte) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("safefs: %w", err)
	}
	defer r.Close()
	rel = filepath.FromSlash(rel)
	if err := mkdirAll(r, filepath.Dir(rel)); err != nil {
		return err
	}
	f, err := r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("safefs: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("safefs: %w", err)
	}
	return f.Close()
}

// MkdirAll creates rel (and its parents) below root via os.Root.
func MkdirAll(root, rel string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("safefs: %w", err)
	}
	defer r.Close()
	return mkdirAll(r, filepath.FromSlash(rel))
}

// ExcludeFromGit appends line to <clone>/.git/info/exclude once. The clone's
// .git comes from the MR and is mounted into checker containers, so the file
// is opened through os.Root: a .git, .git/info or exclude symlink pointing
// outside the clone is refused instead of followed. Best effort: errors are
// ignored.
func ExcludeFromGit(clone, line string) {
	// Steps of one review run concurrently (review.parallel_analyzers) and
	// may add their lines to the same file: read-check-append under a lock.
	excludeMu.Lock()
	defer excludeMu.Unlock()
	r, err := os.OpenRoot(clone)
	if err != nil {
		return
	}
	defer r.Close()
	if fi, err := r.Lstat(".git"); err != nil || !fi.IsDir() {
		return
	}
	excl := filepath.Join(".git", "info", "exclude")
	var b []byte
	if f, err := r.Open(excl); err == nil {
		b, err = io.ReadAll(io.LimitReader(f, maxExclude))
		f.Close()
		if err != nil {
			return
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == line {
			return
		}
	}
	if err := mkdirAll(r, filepath.Join(".git", "info")); err != nil {
		return
	}
	f, err := r.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		_, _ = f.WriteString("\n")
	}
	_, _ = f.WriteString(line + "\n")
}

// excludeMu serializes ExcludeFromGit.
var excludeMu sync.Mutex

// maxExclude bounds how much of .git/info/exclude is read.
const maxExclude = 1 << 20

// mkdirAll creates every component of rel inside r (os.Root has no MkdirAll
// in Go 1.24). Existing components must be directories inside the root.
func mkdirAll(r *os.Root, rel string) error {
	rel = filepath.Clean(rel)
	if rel == "." {
		return nil
	}
	cur := ""
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, c)
		if err := r.Mkdir(cur, 0o755); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("safefs: %w", err)
			}
			// Stat follows in-root links only (os.Root): it must be a dir.
			fi, err := r.Stat(cur)
			if err != nil {
				return fmt.Errorf("safefs: %w", err)
			}
			if !fi.IsDir() {
				return fmt.Errorf("safefs: %s is not a directory", cur)
			}
		}
	}
	return nil
}

// realDir checks that root is a real directory (not a symlink) and returns
// its fully resolved path.
func realDir(root string) (string, error) {
	fi, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("safefs: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("safefs: %s is not a directory", root)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("safefs: %w", err)
	}
	return real, nil
}

// within verifies that p resolves exactly to want, a path inside realRoot.
func within(realRoot, p, want string) error {
	got, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("safefs: %w", err)
	}
	rel, err := filepath.Rel(realRoot, got)
	if err != nil || got != want || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("safefs: %s resolves outside the clone", p)
	}
	return nil
}
