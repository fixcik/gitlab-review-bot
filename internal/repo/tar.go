package repo

import (
	"archive/tar"
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// skipSnapshotEntry drops files that would configure tools rather than be
// read as code: Claude Code project config at the root of the repository
// (already ignored thanks to --setting-sources user; dropped as defense in
// depth) and anything named .git.
func skipSnapshotEntry(name string) bool {
	first, _, _ := strings.Cut(name, "/")
	if first == ".claude" || name == ".mcp.json" {
		return true
	}
	for _, s := range strings.Split(name, "/") {
		if strings.EqualFold(s, ".git") {
			return true
		}
	}
	return false
}

// extractTar unpacks a `git archive` stream into root. Only directories,
// regular files and symlinks are created; names escaping root are rejected.
// Symlinks are created last, so no file is ever written through one.
// Regular files are made read-only. Returns the total size of regular files.
func extractTar(r io.Reader, root string, maxBytes int64) (int64, error) {
	tr := tar.NewReader(bufio.NewReaderSize(r, 1<<16))
	var size int64
	type link struct{ name, target string }
	var links []link
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return size, fmt.Errorf("repo: read archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." || name == "" {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return size, fmt.Errorf("repo: archive entry escapes root: %q", hdr.Name)
		}
		if skipSnapshotEntry(name) {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return size, fmt.Errorf("repo: %w", err)
			}
		case tar.TypeReg:
			size += hdr.Size
			if maxBytes > 0 && size > maxBytes {
				return size, ErrSnapshotTooLarge
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return size, fmt.Errorf("repo: %w", err)
			}
			mode := os.FileMode(0o444)
			if hdr.Mode&0o111 != 0 {
				mode = 0o555
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return size, fmt.Errorf("repo: %w", err)
			}
			_, cerr := io.Copy(f, tr)
			if err := f.Close(); cerr == nil {
				cerr = err
			}
			if cerr != nil {
				return size, fmt.Errorf("repo: write %s: %w", name, cerr)
			}
			if err := os.Chmod(target, mode); err != nil {
				return size, fmt.Errorf("repo: %w", err)
			}
		case tar.TypeSymlink:
			links = append(links, link{target, hdr.Linkname})
		default:
			// Global pax header (commit id), hard links, devices: skip.
		}
	}
	for _, l := range links {
		if err := os.MkdirAll(filepath.Dir(l.name), 0o755); err != nil {
			return size, fmt.Errorf("repo: %w", err)
		}
		if _, err := os.Lstat(l.name); err == nil {
			continue
		}
		if err := os.Symlink(l.target, l.name); err != nil {
			return size, fmt.Errorf("repo: symlink: %w", err)
		}
	}
	return size, nil
}
