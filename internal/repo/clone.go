package repo

// A disposable, self-contained clone of the mirror per review run (the
// checkout used when the tree cache is off; see cached.go for the other).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// placeholderRemote is the origin URL of the bot's clones: never the
// mirror, never a URL with credentials.
func placeholderRemote(projectPath string) string {
	return "https://reviewbot.invalid/" + strings.Trim(projectPath, "/") + ".git"
}

// clone makes a self-contained clone of the mirror in a fresh work dir.
func (m *Manager) clone(ctx context.Context, mirror, projectPath string, mr *core.MergeRequest, shas []string) (_ *Checkout, err error) {
	name, err := randomName()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(m.workDir, name)
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	// --local without --shared: objects are hardlinked (or copied across
	// devices) into a real .git dir; no alternates file pointing back.
	if _, err := m.git.run(ctx, gitCmd{dir: m.workDir, args: []string{
		"clone", "--no-checkout", "--local", "--no-tags", "--quiet", "--", mirror, dir,
	}}); err != nil {
		return nil, fmt.Errorf("repo: clone: %w", err)
	}
	// Bring the bot's refs (MR head, directly fetched SHAs) along so every
	// needed commit is reachable from a ref in the clone.
	if _, err := m.git.run(ctx, gitCmd{dir: dir, args: fetchArgs(mirror, "+refs/reviewbot/*:refs/reviewbot/*")}); err != nil {
		return nil, fmt.Errorf("repo: fetch refs into clone: %w", err)
	}
	for _, sha := range shas {
		if m.git.hasCommit(ctx, dir, sha) {
			continue
		}
		if _, err := m.git.run(ctx, gitCmd{dir: dir, args: fetchArgs(mirror, shaRefspec(sha))}); err != nil {
			return nil, fmt.Errorf("repo: fetch %s into clone: %w", sha, err)
		}
	}
	// Before the checkout: the MR's .gitattributes must not convert, expand
	// or re-encode the files Claude reads, nor hide code from later diffs.
	if err := neutralizeAttributes(filepath.Join(dir, ".git")); err != nil {
		return nil, err
	}
	if _, err := m.git.run(ctx, gitCmd{dir: dir, args: []string{"checkout", "--quiet", "--detach", mr.DiffRefs.HeadSHA}}); err != nil {
		return nil, fmt.Errorf("repo: checkout: %w", err)
	}
	if _, err := m.git.run(ctx, gitCmd{dir: dir, args: []string{"remote", "set-url", "origin", placeholderRemote(projectPath)}}); err != nil {
		return nil, fmt.Errorf("repo: set origin: %w", err)
	}
	// Make sure nothing refers back to the mirror.
	_ = os.Remove(filepath.Join(dir, ".git", "objects", "info", "alternates"))
	// A checked-out file may be a symlink whose target points outside the
	// clone (e.g. ../../etc/passwd or /root/.ssh): an allowed read tool
	// following it would leak host files into the review. Neutralize them.
	if n := neutralizeSymlinks(dir, m.log); n > 0 {
		m.log.Warn("neutralized out-of-tree symlinks in checkout", "project", projectPath, "count", n)
	}
	return &Checkout{dir: dir, git: m.git, log: m.log}, nil
}
