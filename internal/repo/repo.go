// Package repo implements core.Repos: a bare mirror cache per GitLab project
// plus, per review run, either a cached read-only tree of the head
// (treecache.go, Options.CacheTTL > 0) or a disposable, self-contained clone.
//
// Layout under BaseDir:
//
//	mirrors/<sanitized project path>.git   bare mirror, fetched from GitLab
//	trees/<project>@<sha>[-base]           cached read-only tree (no .git; treecache.go)
//	work/<random>                          disposable clone (real .git dir), or a
//	                                       cached checkout's scratch dir (cached.go)
//	snapshots/<project>@<sha>              read-only export, no .git (snapshots.go)
//
// The GitLab token is only ever passed to git through the per-process
// environment (GIT_CONFIG_COUNT/http.extraHeader); it is never written to a
// git config, a remote URL or a log line.
package repo

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Options configures a Manager.
type Options struct {
	// BaseDir holds mirrors and work clones (usually <data_dir>/repos).
	BaseDir string
	// GitLabURL is the GitLab base URL (https://gitlab.example.com). A local
	// filesystem path or file:// URL is used as the remote directly (tests).
	GitLabURL string
	// Token is the GitLab PAT used for fetching over HTTP(S).
	Token string
	// CAFile is a PEM CA bundle for HTTPS fetches from GitLab
	// (gitlab.ca_file); empty uses the system store.
	CAFile string
	Logger *slog.Logger
	// GitBin is the git executable; default "git".
	GitBin string
	// CacheTTL > 0 turns on the checkout cache (treecache.go): read-only
	// trees per (project, commit) under trees/, kept CacheTTL after their
	// last use; CacheMaxBytes > 0 is its disk budget (LRU eviction).
	// 0 = a fresh clone per checkout (the old behaviour).
	CacheTTL      time.Duration
	CacheMaxBytes int64
	// NoJanitor: no background sweep (tests sweep by hand).
	NoJanitor bool
	// HideEnv are further variables of the service environment host git
	// never inherits (case-insensitive), besides core.SecretEnv: the
	// ${NAME} variables of tracing.headers.
	HideEnv []string
}

// Manager implements core.Repos.
type Manager struct {
	baseDir   string
	mirrorDir string
	workDir   string
	gitlabURL string
	local     bool // gitlabURL is a local path
	git       *gitRunner
	log       *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// snaps holds read-only snapshots of context repositories (snapshots.go).
	snaps *snapshotStore
	// trees is the checkout cache (treecache.go); nil = fresh clones.
	trees *treeCache
}

var _ core.Repos = (*Manager)(nil)

// staleWorkAge: work dirs older than this are removed at startup (left over
// from a crash).
const staleWorkAge = 24 * time.Hour

// New creates the directory layout and returns a Manager.
func New(o Options) (*Manager, error) {
	if o.BaseDir == "" {
		return nil, errors.New("repo: BaseDir is required")
	}
	if o.GitLabURL == "" {
		return nil, errors.New("repo: GitLabURL is required")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.GitBin == "" {
		o.GitBin = "git"
	}
	base, err := filepath.Abs(o.BaseDir)
	if err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}
	if o.CAFile != "" {
		// git runs in the mirror directory: a relative path must not be
		// resolved against it.
		if o.CAFile, err = filepath.Abs(o.CAFile); err != nil {
			return nil, fmt.Errorf("repo: ca file: %w", err)
		}
	}
	m := &Manager{
		baseDir:   base,
		mirrorDir: filepath.Join(base, "mirrors"),
		workDir:   filepath.Join(base, "work"),
		git:       &gitRunner{bin: o.GitBin, token: o.Token, caFile: o.CAFile, hide: append([]string(nil), o.HideEnv...)},
		log:       o.Logger,
		locks:     map[string]*sync.Mutex{},
	}
	m.gitlabURL, m.local = normalizeRemote(o.GitLabURL)
	for _, d := range []string{m.mirrorDir, m.workDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("repo: %w", err)
		}
	}
	m.cleanStaleWork()
	if o.CacheTTL > 0 {
		if m.trees, err = newTreeCache(filepath.Join(base, "trees"), o.CacheTTL, o.CacheMaxBytes, m.log, !o.NoJanitor); err != nil {
			return nil, fmt.Errorf("repo: %w", err)
		}
	}
	if m.snaps, err = newSnapshotStore(filepath.Join(base, "snapshots")); err != nil {
		return nil, fmt.Errorf("repo: %w", err)
	}
	return m, nil
}

// normalizeRemote reports whether u points to the local filesystem and
// returns it in a usable form (absolute path for local remotes).
func normalizeRemote(u string) (string, bool) {
	if strings.HasPrefix(u, "file://") {
		if pu, err := url.Parse(u); err == nil {
			return filepath.Clean(pu.Path), true
		}
		return strings.TrimPrefix(u, "file://"), true
	}
	if strings.Contains(u, "://") {
		return strings.TrimRight(u, "/"), false
	}
	if abs, err := filepath.Abs(u); err == nil {
		return abs, true
	}
	return u, true
}

// remoteURL is the fetch URL for a project. It never contains credentials.
func (m *Manager) remoteURL(projectPath string) string {
	if m.local {
		// Prefer <base>/<project>.git or <base>/<project> when it exists,
		// otherwise the base path itself is the repository.
		for _, c := range []string{
			filepath.Join(m.gitlabURL, filepath.FromSlash(projectPath)+".git"),
			filepath.Join(m.gitlabURL, filepath.FromSlash(projectPath)),
		} {
			if isDir(c) {
				return c
			}
		}
		return m.gitlabURL
	}
	return m.gitlabURL + "/" + strings.Trim(projectPath, "/") + ".git"
}

// sanitize turns "group/sub/project" into a safe single path component that
// is unique per project path.
func sanitize(projectPath string) string {
	var b strings.Builder
	for _, r := range projectPath {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := strings.Trim(b.String(), "._")
	if len(name) > 80 {
		name = name[:80]
	}
	sum := sha1.Sum([]byte(projectPath))
	return name + "-" + hex.EncodeToString(sum[:4])
}

func (m *Manager) projectLock(projectPath string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[projectPath]
	if !ok {
		l = &sync.Mutex{}
		m.locks[projectPath] = l
	}
	return l
}

// lockCtx takes l, giving up when ctx is done (another run may hold the
// project lock for a long fetch or checkout).
func lockCtx(ctx context.Context, l *sync.Mutex) error {
	for !l.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

func (m *Manager) cleanStaleWork() {
	entries, err := os.ReadDir(m.workDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < staleWorkAge {
			continue
		}
		p := filepath.Join(m.workDir, e.Name())
		if err := os.RemoveAll(p); err == nil {
			m.log.Info("removed stale work dir", "dir", p)
		}
	}
}

// Checkout implements core.Repos.
func (m *Manager) Checkout(ctx context.Context, projectPath string, mr *core.MergeRequest) (core.Checkout, error) {
	if mr == nil {
		return nil, errors.New("repo: nil merge request")
	}
	head := mr.DiffRefs.HeadSHA
	if head == "" {
		return nil, errors.New("repo: merge request has no head SHA")
	}
	shas := uniqueNonEmpty(mr.DiffRefs.BaseSHA, mr.DiffRefs.StartSHA, head)
	for _, s := range shas {
		if !validRev(s) {
			return nil, fmt.Errorf("repo: invalid SHA %q", s)
		}
	}
	if mr.TargetBranch != "" && !validRefName(mr.TargetBranch) {
		return nil, fmt.Errorf("repo: invalid target branch %q", mr.TargetBranch)
	}

	mirror := m.mirrorPath(projectPath)
	start := time.Now()
	lock := m.projectLock(projectPath)
	lock.Lock()
	if err := m.updateMirror(ctx, mirror, projectPath, mr, shas); err != nil {
		lock.Unlock()
		return nil, err
	}
	if m.trees == nil {
		co, err := m.clone(ctx, mirror, projectPath, mr, shas)
		lock.Unlock()
		if err != nil {
			return nil, err
		}
		m.log.Info("checkout ready", "project", projectPath, "mr", mr.IID, "head", head,
			"dir", co.dir, "took", time.Since(start).Round(time.Millisecond))
		return co, nil
	}
	// The tree is built (or reused) outside the project lock: other jobs of
	// the project may fetch meanwhile; git reads stay consistent and objects
	// are never removed by a fetch.
	lock.Unlock()
	co, err := m.cachedCheckout(ctx, mirror, projectPath, mr)
	if err != nil {
		return nil, err
	}
	m.log.Info("checkout ready", "project", projectPath, "mr", mr.IID, "head", head,
		"dir", co.dir, "cached_tree", true, "took", time.Since(start).Round(time.Millisecond))
	return co, nil
}

// isDir reports whether p is an existing directory.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func randomName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("repo: random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
