package repo

// The bare mirror of a project: created on first use, fetched per review
// (target branch, MR head and any commit still missing).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// mirrorPath is the bare mirror of a project.
func (m *Manager) mirrorPath(projectPath string) string {
	return filepath.Join(m.mirrorDir, sanitize(projectPath)+".git")
}

// ensureMirror creates the bare mirror when it does not exist yet and
// reports whether it did. The mirror (also one made by an older version)
// gets the neutral attributes (attributes.go).
func (m *Manager) ensureMirror(ctx context.Context, mirror string) (bool, error) {
	created := false
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		if err := os.MkdirAll(mirror, 0o755); err != nil {
			return false, fmt.Errorf("repo: %w", err)
		}
		if _, err := m.git.run(ctx, gitCmd{dir: mirror, args: []string{"init", "--bare", "-q"}}); err != nil {
			return false, fmt.Errorf("repo: init mirror: %w", err)
		}
		created = true
	}
	if err := neutralizeAttributes(mirror); err != nil {
		return created, err
	}
	return created, nil
}

// updateMirror creates (if needed) and fetches the bare mirror.
func (m *Manager) updateMirror(ctx context.Context, mirror, projectPath string, mr *core.MergeRequest, shas []string) error {
	created, err := m.ensureMirror(ctx, mirror)
	if err != nil {
		return err
	}
	if created {
		m.log.Info("created mirror", "project", projectPath, "dir", mirror)
	}

	remote := m.remoteURL(projectPath)
	auth := !m.local
	refspecs := []string{fmt.Sprintf("+refs/merge-requests/%d/head:refs/reviewbot/mr/%d", mr.IID, mr.IID)}
	if mr.TargetBranch != "" {
		refspecs = append([]string{fmt.Sprintf("+refs/heads/%s:refs/heads/%s", mr.TargetBranch, mr.TargetBranch)}, refspecs...)
	}
	if _, err := m.git.run(ctx, gitCmd{dir: mirror, args: fetchArgs(remote, refspecs...), auth: auth, timeout: networkTimeout}); err != nil {
		// A missing target branch (deleted/renamed) must not prevent a
		// review: retry with the MR ref only, SHAs are verified below.
		if len(refspecs) == 1 {
			return fmt.Errorf("repo: fetch %s: %w", projectPath, err)
		}
		m.log.Warn("fetch failed, retrying with MR ref only", "project", projectPath, "err", err)
		mrRef := refspecs[len(refspecs)-1]
		if _, err := m.git.run(ctx, gitCmd{dir: mirror, args: fetchArgs(remote, mrRef), auth: auth, timeout: networkTimeout}); err != nil {
			return fmt.Errorf("repo: fetch %s: %w", projectPath, err)
		}
	}

	for _, sha := range shas {
		if m.git.hasCommit(ctx, mirror, sha) {
			continue
		}
		m.log.Info("commit missing in mirror, fetching it directly", "project", projectPath, "sha", sha)
		args := fetchArgs(remote, shaRefspec(sha))
		if _, err := m.git.run(ctx, gitCmd{dir: mirror, args: args, auth: auth, timeout: networkTimeout}); err != nil {
			return fmt.Errorf("repo: fetch commit %s of %s: %w", sha, projectPath, err)
		}
		if !m.git.hasCommit(ctx, mirror, sha) {
			return fmt.Errorf("repo: commit %s not found in %s", sha, projectPath)
		}
	}
	return nil
}

// shaRefspec keeps a directly fetched commit reachable under refs/reviewbot/sha/.
func shaRefspec(sha string) string {
	return fmt.Sprintf("+%s:refs/reviewbot/sha/%s", sha, sha)
}
