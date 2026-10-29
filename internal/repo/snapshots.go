package repo

// Read-only snapshots of other repositories ("context repositories").
//
// Layout under BaseDir:
//
//	snapshots/<sanitized project path>@<sha>   exported tree, no .git
//	snapshots/.tmp-<random>                    export in progress
//	snapshots/.trash-<random>                  being removed
//
// A snapshot is exported from the same bare mirror the reviews use (same
// auth mechanism and forced safe git config), with `git archive` streamed
// into a Go tar reader: no working tree, no index, no .git. Out-of-tree
// symlinks are neutralized like in review checkouts.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SnapshotSpec names one repository to snapshot.
type SnapshotSpec struct {
	// Path is the GitLab path_with_namespace, e.g. "billing/api".
	Path string
	// DefaultBranch to export; empty means ask the remote
	// (`git ls-remote --symref HEAD`).
	DefaultBranch string
}

// SnapshotOptions control fetching and limits.
type SnapshotOptions struct {
	// RefreshInterval: a repository is fetched at most this often; in
	// between, the current snapshot (or the last error) is returned. <= 0
	// fetches on every call.
	RefreshInterval time.Duration
	// MaxBytes skips repositories whose exported tree is larger. 0 = no limit.
	MaxBytes int64
	// Concurrency of Snapshots; default 4.
	Concurrency int
}

// Snapshot is one exported tree in use by a run. Call Release when the run
// is over; superseded snapshots are deleted once nobody uses them.
type Snapshot struct {
	Path   string // project path
	Branch string // default branch it was exported from
	SHA    string
	Dir    string // absolute host path
	Size   int64  // bytes of regular files

	store *snapshotStore
	once  sync.Once
}

// Release marks the snapshot as no longer used by this run. Safe to call
// more than once.
func (s *Snapshot) Release() {
	if s == nil || s.store == nil {
		return
	}
	s.once.Do(func() { s.store.release(s.Path, s.Dir) })
}

// ErrSnapshotTooLarge means the repository exceeds SnapshotOptions.MaxBytes.
var ErrSnapshotTooLarge = errors.New("repo: repository exceeds the context size limit")

// Snapshots exports every spec concurrently and returns the successful
// snapshots in spec order. Failures are logged and skipped: a broken context
// repository never fails the caller.
func (m *Manager) Snapshots(ctx context.Context, specs []SnapshotSpec, o SnapshotOptions) []*Snapshot {
	n := o.Concurrency
	if n <= 0 {
		n = 4
	}
	res := make([]*Snapshot, len(specs))
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i, sp := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			s, err := m.Snapshot(ctx, sp, o)
			if err != nil {
				if ctx.Err() == nil {
					m.log.Warn("context repository skipped", "project", sp.Path, "err", err)
				}
				return
			}
			res[i] = s
		}()
	}
	wg.Wait()
	out := res[:0]
	for _, s := range res {
		if s != nil {
			out = append(out, s)
		}
	}
	return out
}

// Snapshot returns a read-only export of the default branch of spec.Path,
// fetching it when the last fetch is older than o.RefreshInterval.
func (m *Manager) Snapshot(ctx context.Context, spec SnapshotSpec, o SnapshotOptions) (*Snapshot, error) {
	p := strings.Trim(strings.TrimSpace(spec.Path), "/")
	if !ValidProjectPath(p) {
		return nil, fmt.Errorf("repo: invalid project path %q", spec.Path)
	}
	if spec.DefaultBranch != "" && !validRefName(spec.DefaultBranch) {
		return nil, fmt.Errorf("repo: invalid default branch %q", spec.DefaultBranch)
	}
	st := m.snaps

	lock := m.projectLock(p)
	if err := lockCtx(ctx, lock); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	if s, err, ok := st.cached(p, spec.DefaultBranch, o.RefreshInterval); ok {
		return s, err
	}
	s, err := m.refreshSnapshot(ctx, p, spec.DefaultBranch, o.MaxBytes)
	if err != nil {
		if ctx.Err() != nil {
			// Cancelled: do not remember it as the repo's state.
			return nil, err
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		rs := st.state(p)
		rs.fetchedAt = st.now()
		if !errors.Is(err, ErrSnapshotTooLarge) && rs.dir != "" && (spec.DefaultBranch == "" || spec.DefaultBranch == rs.branch) {
			if _, serr := os.Stat(rs.dir); serr == nil {
				// GitLab hiccup: keep serving the last good snapshot (its
				// SHA is shown to Claude) until the next refresh.
				m.log.Warn("context repository refresh failed, using the previous snapshot", "project", p, "sha", rs.sha, "err", err)
				rs.err = nil
				return st.acquireLocked(p, rs), nil
			}
		}
		rs.err, rs.errBranch = err, spec.DefaultBranch
		return nil, err
	}
	return s, nil
}

// refreshSnapshot fetches the default branch into the mirror and makes sure
// a snapshot of its tip exists. Called with the project lock held.
func (m *Manager) refreshSnapshot(ctx context.Context, p, branch string, maxBytes int64) (*Snapshot, error) {
	st := m.snaps
	remote := m.remoteURL(p)
	auth := !m.local
	if branch == "" {
		b, err := m.remoteHead(ctx, remote, auth)
		if err != nil {
			return nil, fmt.Errorf("repo: default branch of %s: %w", p, err)
		}
		branch = b
	}

	mirror := m.mirrorPath(p)
	if _, err := m.ensureMirror(ctx, mirror); err != nil {
		return nil, err
	}
	ref := "refs/heads/" + branch
	if _, err := m.git.run(ctx, gitCmd{dir: mirror, auth: auth, timeout: networkTimeout, args: fetchArgs(remote, "+"+ref+":"+ref)}); err != nil {
		return nil, fmt.Errorf("repo: fetch %s of %s: %w", branch, p, err)
	}
	out, err := m.git.run(ctx, gitCmd{dir: mirror, args: []string{"rev-parse", "--verify", "--quiet", ref + "^{commit}"}})
	if err != nil {
		return nil, fmt.Errorf("repo: resolve %s of %s: %w", branch, p, err)
	}
	sha := strings.TrimSpace(string(out))
	if !validRev(sha) {
		return nil, fmt.Errorf("repo: bad sha %q for %s", sha, p)
	}

	prefix := sanitize(p) + "@"
	dir := filepath.Join(st.dir, prefix+sha)

	st.mu.Lock()
	rs := st.state(p)
	if rs.tooLarge == sha {
		// Same commit was already too large: don't export it again.
		err := fmt.Errorf("%w: %s", ErrSnapshotTooLarge, p)
		st.mu.Unlock()
		return nil, err
	}
	st.mu.Unlock()

	size, err := m.snapshotOf(ctx, p, branch, mirror, sha, dir, maxBytes)
	if err != nil {
		if errors.Is(err, ErrSnapshotTooLarge) {
			st.mu.Lock()
			rs.tooLarge = sha
			st.mu.Unlock()
		}
		return nil, err
	}

	st.mu.Lock()
	rs.branch, rs.sha, rs.dir, rs.size = branch, sha, dir, size
	rs.fetchedAt, rs.err = st.now(), nil
	s := st.acquireLocked(p, rs)
	trash := st.cleanupLocked(prefix, dir)
	st.mu.Unlock()
	for _, t := range trash {
		_ = os.RemoveAll(t)
	}
	return s, nil
}

// snapshotOf returns the size of the snapshot of sha in dir, exporting it
// first when it does not exist yet.
func (m *Manager) snapshotOf(ctx context.Context, p, branch, mirror, sha, dir string, maxBytes int64) (int64, error) {
	if isDir(dir) {
		size := m.snaps.knownSize(p, sha, dir)
		if maxBytes > 0 && size > maxBytes {
			return 0, fmt.Errorf("%w: %s is %d MB", ErrSnapshotTooLarge, p, size>>20)
		}
		return size, nil
	}
	size, err := m.exportSnapshot(ctx, mirror, sha, dir, maxBytes)
	if errors.Is(err, ErrSnapshotTooLarge) {
		return 0, fmt.Errorf("%w: %s", err, p)
	}
	if err != nil {
		return 0, err
	}
	m.log.Info("context snapshot created", "project", p, "branch", branch, "sha", sha, "size_mb", size>>20)
	return size, nil
}

// remoteHead asks the remote for its default branch.
func (m *Manager) remoteHead(ctx context.Context, remote string, auth bool) (string, error) {
	out, err := m.git.run(ctx, gitCmd{dir: m.baseDir, auth: auth, timeout: networkTimeout,
		args: []string{"ls-remote", "--symref", "--", remote, "HEAD"}})
	if err != nil {
		return "", err
	}
	return parseSymrefHead(string(out))
}

// parseSymrefHead extracts the branch from `git ls-remote --symref HEAD`
// output ("ref: refs/heads/main\tHEAD").
func parseSymrefHead(out string) (string, error) {
	for _, l := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(l, "ref: refs/heads/")
		if !ok {
			continue
		}
		b, _, _ := strings.Cut(rest, "\t")
		b = strings.TrimSpace(b)
		if validRefName(b) {
			return b, nil
		}
		return "", fmt.Errorf("invalid branch %q", b)
	}
	return "", errors.New("remote has no HEAD (empty repository?)")
}

// exportSnapshot writes the tree of sha into dir (atomically, via a temp dir
// and rename) and returns its size.
func (m *Manager) exportSnapshot(ctx context.Context, mirror, sha, dir string, maxBytes int64) (int64, error) {
	name, err := randomName()
	if err != nil {
		return 0, err
	}
	tmp := filepath.Join(m.snaps.dir, ".tmp-"+name)
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return 0, fmt.Errorf("repo: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := neutralizeAttributes(mirror); err != nil {
		return 0, err
	}
	var size int64
	err = m.git.stream(ctx, gitCmd{dir: mirror, args: []string{"archive", "--format=tar", sha}}, func(r io.Reader) error {
		var xerr error
		size, xerr = extractTar(r, tmp, maxBytes)
		return xerr
	})
	if err != nil {
		return 0, err
	}
	if n := neutralizeSymlinks(tmp, m.log); n > 0 {
		m.log.Warn("neutralized out-of-tree symlinks in context snapshot", "dir", dir, "count", n)
	}
	if err := os.Rename(tmp, dir); err != nil {
		if _, statErr := os.Stat(dir); statErr == nil {
			// Someone else won the race: use theirs.
			return m.snaps.knownSize("", "", dir), nil
		}
		return 0, fmt.Errorf("repo: %w", err)
	}
	ok = true
	return size, nil
}
