package core

import (
	"context"
	"errors"
)

// Repos manages local clones.
type Repos interface {
	// Checkout makes a working tree of mr.DiffRefs.HeadSHA with the base and
	// start commits available for diffing.
	Checkout(ctx context.Context, projectPath string, mr *MergeRequest) (Checkout, error)
}

// Checkout is one prepared working tree. Close removes it.
type Checkout interface {
	Dir() string
	// Diff returns a unified diff from..HEAD (three-dot semantics are NOT
	// applied; from must be an ancestor or a merge base). Paths matching any
	// ignore glob (doublestar syntax) are excluded.
	Diff(ctx context.Context, from string, ignore []string) (string, error)
	// ChangedFiles lists paths changed from..HEAD, ignore applied.
	ChangedFiles(ctx context.Context, from string, ignore []string) ([]string, error)
	// IsAncestor reports whether commit is an ancestor of HEAD (false if the
	// commit is unknown, e.g. after a force push).
	IsAncestor(ctx context.Context, commit string) (bool, error)
	// ShowFile returns the content of path in commit rev (like
	// `git show rev:path`, but never through textconv/filters). ErrNotFound
	// when the path does not exist in that commit.
	ShowFile(ctx context.Context, rev, path string) ([]byte, error)
	// ListFiles lists every file path in the tree of commit rev.
	ListFiles(ctx context.Context, rev string) ([]string, error)
	Close() error
}

// GrepMatch is one line WordGrepper found.
type GrepMatch struct {
	Path string
	Line int
	Text string
}

// WordGrepper is optionally implemented by a Checkout (found by a type
// assertion): a read-only whole-word search for fixed strings in the tree
// of commit rev (`git grep -w -F`), limited to pathspecs (git pathspec
// syntax, e.g. "*.php"). Words are identifiers ([A-Za-z0-9_$]). Output
// beyond maxBytes is dropped and reported as truncated. No match is not an
// error.
type WordGrepper interface {
	GrepWords(ctx context.Context, rev string, words, pathspecs []string, maxBytes int) (matches []GrepMatch, truncated bool, err error)
}

// ErrTreeTooLarge means an exported tree would exceed the size limit.
var ErrTreeTooLarge = errors.New("tree exceeds the size limit")

// TreeExporter is optionally implemented by a Checkout (found by a type
// assertion): ExportTree returns a directory on the host with a read-only
// copy of the tree of commit rev (regular files 0444, no .git, no .claude/
// or .mcp.json, out-of-tree symlinks removed). It is a new directory, or,
// with the checkout cache (internal/repo), a cached tree shared with other
// jobs: never write into it. release gives it up (removes a new directory;
// a cached tree stays for its TTL); Close of the checkout releases it at
// the latest. maxBytes > 0 caps the total size of regular files
// (ErrTreeTooLarge).
type TreeExporter interface {
	ExportTree(ctx context.Context, rev string, maxBytes int64) (dir string, release func(), err error)
}

// CheckoutAs finds an optional capability T (ExportTree, GrepWords, …) on
// co or on any checkout it wraps (via an `Unwrap() Checkout` method, as the
// diagnostics decorator provides). A plain type assertion on a decorated
// checkout would silently report the capability as missing.
func CheckoutAs[T any](co Checkout) (T, bool) {
	for i := 0; co != nil && i < 8; i++ {
		if t, ok := co.(T); ok {
			return t, true
		}
		u, ok := co.(interface{ Unwrap() Checkout })
		if !ok {
			break
		}
		co = u.Unwrap()
	}
	var zero T
	return zero, false
}
