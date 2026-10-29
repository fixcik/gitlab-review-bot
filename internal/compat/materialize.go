package compat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/safefs"
)

// sides are the materialized versions: "old" (base) and "new" (head).
var sides = []string{"old", "new"}

// materializer writes base files under <scratch>/old and head files under
// <scratch>/new, keeping repository paths.
type materializer struct {
	co         core.Checkout
	base, head string
	dir        string // absolute scratch dir
	written    map[string]bool
	count      map[string]int
	bytes      map[string]int
	lists      map[string][]string
}

func newMaterializer(co core.Checkout, base, head string) (*materializer, error) {
	// A cached checkout's tree is shared and read-only: the checkout hands
	// out a per-job scratch dir outside it (repo.Checkout.ScratchDir).
	// Otherwise the clone is the MR's: a symlink committed at ScratchDir must
	// not redirect our writes outside it (safefs replaces it with a real dir).
	var dir string
	var err error
	if sc, ok := core.CheckoutAs[scratcher](co); ok {
		if dir, err = sc.ScratchDir(ScratchDir); err != nil {
			return nil, fmt.Errorf("compat: scratch dir: %w", err)
		}
	}
	inClone := dir == ""
	if inClone {
		if dir, err = safefs.PrepareDir(co.Dir(), ScratchDir, true); err != nil {
			return nil, fmt.Errorf("compat: scratch dir: %w", err)
		}
	}
	for _, side := range sides {
		if err := safefs.MkdirAll(dir, side); err != nil {
			return nil, fmt.Errorf("compat: scratch dir: %w", err)
		}
	}
	if inClone {
		safefs.ExcludeFromGit(co.Dir(), "/"+ScratchDir+"/")
	}
	return &materializer{co: co, base: base, head: head, dir: dir,
		written: map[string]bool{}, count: map[string]int{}, bytes: map[string]int{}, lists: map[string][]string{}}, nil
}

func (m *materializer) cleanup() { _ = os.RemoveAll(m.dir) }

// scratcher is optionally implemented by a checkout (repo.Checkout): a
// fresh writable directory named name outside the checkout's tree ("" =
// none: use the clone).
type scratcher interface {
	ScratchDir(name string) (string, error)
}

func (m *materializer) rev(side string) string {
	if side == "old" {
		return m.base
	}
	return m.head
}

// files lists every file of the side's commit (cached).
func (m *materializer) files(ctx context.Context, side string) ([]string, error) {
	if l, ok := m.lists[side]; ok {
		return l, nil
	}
	l, err := m.co.ListFiles(ctx, m.rev(side))
	if err != nil {
		return nil, err
	}
	m.lists[side] = l
	return l, nil
}

var errTooMany = errors.New("too many files")

// write copies one file of side into the scratch dir. Missing files are
// ignored.
func (m *materializer) write(ctx context.Context, side, p string) error {
	key := side + "/" + p
	if m.written[key] {
		return nil
	}
	if !safeRel(p) {
		return fmt.Errorf("unsafe path %q", p)
	}
	b, err := m.co.ShowFile(ctx, m.rev(side), p)
	if errors.Is(err, core.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.count[side]+1 > maxFilesPerSide || m.bytes[side]+len(b) > maxBytesPerSide {
		return errTooMany
	}
	// Written through os.Root: no path component may leave the scratch dir.
	if err := safefs.WriteFile(m.dir, side+"/"+p, b); err != nil {
		return err
	}
	m.written[key] = true
	m.count[side]++
	m.bytes[side] += len(b)
	return nil
}

// safeRel accepts clean relative slash paths that stay inside the tree.
func safeRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return false
	}
	c := path.Clean(p)
	return c == p && c != "." && c != ".." && !strings.HasPrefix(c, "../")
}
