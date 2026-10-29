package repo

// Repository attributes (.gitattributes) are data of the reviewed project,
// so they are untrusted: `*.go -diff` (or `binary`, or a macro expanding to
// -diff) turns the MR's code into "Binary files differ" in the diff, in
// numstat and for `git grep -I`; `export-ignore` drops files from exported
// trees and snapshots; `export-subst`, `ident`, `eol`/`text`,
// `working-tree-encoding` and `filter` rewrite the content the bot and Claude
// read. git applies them from the working tree and the index (clone mode),
// from the tree being archived or checked out, and, in a bare repository on
// git >= 2.43, from HEAD (the mirror's default branch).
//
// Every repository the bot runs git in (the mirrors, the clones, the
// history clones) gets neutralAttributes as $GIT_DIR/info/attributes: the
// attribute file with the highest precedence, read by every git version
// (the bot image ships git 2.39, before --attr-source / GIT_ATTR_SOURCE).
// "!name" resets an attribute to unspecified, which no lower-precedence
// .gitattributes line or macro can change. Unspecified `diff` keeps git's
// own binary detection (a NUL byte in the first 8 KB): real binary files are
// still shown as "Binary files differ", never dumped into a prompt. The file
// is rewritten before every git command whose output the bot reads (a clone
// may be mounted writable into an exec container).

import (
	"fmt"
	"os"
	"path/filepath"
)

// neutralAttributes resets, for every path, each attribute that changes what
// git shows or writes: binary detection and diff drivers (diff, the binary
// macro), conversions on checkout and export (text, eol, crlf, filter,
// ident, working-tree-encoding) and archive rules (export-ignore,
// export-subst).
const neutralAttributes = "* !diff !binary !text !eol !crlf !filter !ident !working-tree-encoding !export-ignore !export-subst\n"

// neutralizeAttributes makes <gitDir>/info/attributes exactly
// neutralAttributes. It never writes through a symlink: a non-directory info
// (or gitDir itself) is an error or replaced, the file is renamed into place.
func neutralizeAttributes(gitDir string) error {
	if err := realDir(gitDir); err != nil {
		return err
	}
	info := filepath.Join(gitDir, "info")
	if fi, err := os.Lstat(info); err == nil && !fi.IsDir() {
		_ = os.Remove(info) // a symlink or a file: replaced
	}
	_ = os.Mkdir(info, 0o755) // a failure leaves no directory: realDir reports it
	if err := realDir(info); err != nil {
		return err
	}
	p := filepath.Join(info, "attributes")
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
		if b, err := os.ReadFile(p); err == nil && string(b) == neutralAttributes {
			return nil
		}
	}
	f, err := os.CreateTemp(info, ".attributes-*")
	if err != nil {
		return fmt.Errorf("repo: attributes: %w", err)
	}
	werr := f.Chmod(0o644) // read by git in the sandbox too (gitleaks)
	if werr == nil {
		_, werr = f.WriteString(neutralAttributes)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), p)
	}
	if werr != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("repo: attributes: %w", werr)
	}
	return nil
}

// realDir fails unless p is a directory (not a symlink to one).
func realDir(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("repo: attributes: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("repo: attributes: %s is not a directory", p)
	}
	return nil
}

// gitDir is the git directory of the repository the checkout's git commands
// run in: the clone's .git, or the mirror of a cached checkout.
func (c *Checkout) gitDir() string {
	if c.mgr != nil {
		return c.mirror
	}
	return filepath.Join(c.dir, ".git")
}

// neutralAttrs (re)writes the neutral attributes of the checkout's
// repository; callers fail closed on its error.
func (c *Checkout) neutralAttrs() error {
	return neutralizeAttributes(c.gitDir())
}
