package repo

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// neutralizeSymlinks removes every symlink under root whose target resolves
// outside root, so that tools confined to the clone cannot read or write host
// files by following a link the MR author planted. In-tree symlinks are kept.
// The .git directory is skipped (bot-written, and its internal links such as
// packed refs are not attacker-controlled here). Returns the count removed.
func neutralizeSymlinks(root string, log *slog.Logger) int {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	removed := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		if outsideTree(realRoot, path) {
			if rmErr := os.Remove(path); rmErr != nil {
				if log != nil {
					log.Warn("failed to remove out-of-tree symlink", "path", path, "err", rmErr)
				}
				return nil
			}
			if log != nil {
				target, _ := os.Readlink(path)
				log.Info("removed out-of-tree symlink", "path", path, "target", target)
			}
			removed++
		}
		return nil
	})
	return removed
}

// outsideTree reports whether the symlink at linkPath resolves to a location
// outside realRoot. A link whose target does not exist yet (dangling) is
// judged by the lexical resolution of its target; anything that escapes the
// tree, or cannot be resolved safely, is treated as outside.
func outsideTree(realRoot, linkPath string) bool {
	target, err := os.Readlink(linkPath)
	if err != nil {
		return true
	}
	var resolved string
	if filepath.IsAbs(target) {
		resolved = filepath.Clean(target)
	} else {
		resolved = filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
	}
	// Prefer the fully resolved real path when it exists (collapses any
	// intermediate symlinks); fall back to the lexical resolution otherwise.
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = real
	}
	return resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(os.PathSeparator))
}
