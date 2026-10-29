package review

// Reading .reviewbot.yaml at a ref (cached by raw content and TTL).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
)

// cfgFile is .reviewbot.yaml at one ref.
type cfgFile struct {
	found bool
	raw   []byte
	// tooBig: the file is over config.MaxRepoConfigBytes (raw is nil; it
	// is invalid).
	tooBig bool
	repo   *config.Repo      // parsed (nil when absent or invalid)
	issues config.RepoErrors // validation problems
}

func (f cfgFile) valid() bool { return f.found && len(f.issues) == 0 }

// rawState is the file as read (for sameRaw).
func (f cfgFile) rawState() cfgRaw { return cfgRaw{found: f.found, tooBig: f.tooBig, raw: f.raw} }

// sameRaw reports whether two read files have the same content (a too-big
// file is never "the same": its content is not kept).
func sameRaw(a, b cfgRaw) bool {
	return a.found && b.found && !a.tooBig && !b.tooBig && bytes.Equal(a.raw, b.raw)
}

type cfgCacheKey struct {
	project int64
	ref     string
}

// cfgRaw is the file as read (and cached): absent, too big (content not
// kept) or its bytes.
type cfgRaw struct {
	found, tooBig bool
	raw           []byte
}

type cfgCacheEntry struct {
	cfgRaw
	at time.Time
}

type cfgCache struct {
	mu    sync.Mutex
	m     map[cfgCacheKey]cfgCacheEntry
	bytes int // sum of the cached raw sizes
}

const (
	// maxCfgCache bounds the cache entries and maxCfgCacheBytes the bytes
	// kept (each file is at most config.MaxRepoConfigBytes; larger ones are
	// cached as "too big" without content). Over either bound everything
	// is dropped.
	maxCfgCache      = 2000
	maxCfgCacheBytes = 32 << 20
	// pruneCfgCacheAt: from this many entries every insert drops the
	// expired ones first (entries are keyed by commit SHA too, so they pile
	// up).
	pruneCfgCacheAt = 200
)

// rawFileCapped reads a repository file with a size cap (*gitlab.Client).
type rawFileCapped interface {
	GetRawFileMax(ctx context.Context, projectID int64, path, ref string, max int64) ([]byte, error)
}

// rawRepoConfig reads .reviewbot.yaml at ref (a branch or a commit SHA),
// through the cache. The read is capped just above
// config.MaxRepoConfigBytes when the client supports it; a larger file is
// reported (and cached) as too big, without its content.
func (r *Reviewer) rawRepoConfig(ctx context.Context, projectID int64, ref string) (cfgRaw, error) {
	ttl := r.cfgTTL
	key := cfgCacheKey{projectID, ref}
	now := r.env.Now()
	if ttl > 0 {
		r.cfgCache.mu.Lock()
		e, ok := r.cfgCache.m[key]
		r.cfgCache.mu.Unlock()
		if ok && now.Sub(e.at) < ttl && !now.Before(e.at) {
			return e.cfgRaw, nil
		}
	}
	var b []byte
	var err error
	if rc, ok := core.GitLabAs[rawFileCapped](r.env.GitLab); ok {
		b, err = rc.GetRawFileMax(ctx, projectID, config.RepoConfigPath, ref, config.MaxRepoConfigBytes)
	} else {
		b, err = r.env.GitLab.GetRawFile(ctx, projectID, config.RepoConfigPath, ref)
	}
	var out cfgRaw
	switch {
	case errors.Is(err, core.ErrNotFound):
	case errors.Is(err, core.ErrTooLarge):
		out = cfgRaw{found: true, tooBig: true}
	case err != nil:
		return cfgRaw{}, fmt.Errorf("read %s: %w", config.RepoConfigPath, err)
	case len(b) > config.MaxRepoConfigBytes:
		out = cfgRaw{found: true, tooBig: true} // not kept
	default:
		out = cfgRaw{found: true, raw: b}
	}
	if ttl > 0 {
		r.cfgCache.put(key, cfgCacheEntry{cfgRaw: out, at: now}, ttl)
	}
	return out, nil
}

// put stores e; expired entries are dropped first once the cache holds
// pruneCfgCacheAt entries, everything when a bound is still reached.
func (c *cfgCache) put(key cfgCacheKey, e cfgCacheEntry, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[cfgCacheKey]cfgCacheEntry{}
	}
	if old, ok := c.m[key]; ok {
		delete(c.m, key)
		c.bytes -= len(old.raw)
	}
	if len(c.m) >= pruneCfgCacheAt || c.bytes+len(e.raw) > maxCfgCacheBytes {
		for k, o := range c.m {
			if e.at.Sub(o.at) >= ttl || e.at.Before(o.at) {
				delete(c.m, k)
				c.bytes -= len(o.raw)
			}
		}
	}
	if len(c.m) >= maxCfgCache || c.bytes+len(e.raw) > maxCfgCacheBytes {
		c.m, c.bytes = map[cfgCacheKey]cfgCacheEntry{}, 0
	}
	c.m[key] = e
	c.bytes += len(e.raw)
}

// readRepoConfig reads and validates .reviewbot.yaml at ref.
func (r *Reviewer) readRepoConfig(ctx context.Context, projectID int64, ref string) (cfgFile, error) {
	raw, err := r.rawRepoConfig(ctx, projectID, ref)
	if err != nil || !raw.found {
		return cfgFile{}, err
	}
	if raw.tooBig {
		return cfgFile{found: true, tooBig: true, issues: config.RepoTooLarge()}, nil
	}
	f := cfgFile{found: true, raw: raw.raw}
	f.repo, f.issues = config.ValidateRepo(raw.raw)
	return f, nil
}

// optedIn reports whether a repository whose target branch has (or lacks)
// the config takes part in reviews.
func (r *Reviewer) optedIn(target cfgFile) bool {
	return target.found || !r.env.Cfg.Review.RequireRepoConfig
}

// targetRepoConfig loads the target branch's config of mr: nil repo when
// absent; *repoConfigError when invalid or not opted in.
func (r *Reviewer) targetRepoConfig(ctx context.Context, projectID int64, mr *core.MergeRequest) (*config.Repo, error) {
	t, err := r.readRepoConfig(ctx, projectID, mr.TargetBranch)
	if err != nil {
		return nil, err
	}
	switch {
	case t.found && len(t.issues) > 0:
		return nil, &repoConfigError{err: t.issues, branch: mr.TargetBranch}
	case !r.optedIn(t):
		return nil, &repoConfigError{notOptedIn: true, branch: mr.TargetBranch,
			err: fmt.Errorf("repository not opted in: no %s on %s", config.RepoConfigPath, mr.TargetBranch)}
	}
	return t.repo, nil
}

// headRepoConfig returns the MR's own .reviewbot.yaml when the MR adds or
// changes it (HEAD differs from the merge base); nil when the MR does not
// touch it or removes it.
func (r *Reviewer) headRepoConfig(ctx context.Context, projectID int64, mr *core.MergeRequest, target cfgFile) (*cfgFile, error) {
	head := mr.DiffRefs.HeadSHA
	if head == "" {
		return nil, nil
	}
	h, err := r.readRepoConfig(ctx, projectID, head)
	if err != nil || !h.found {
		return nil, err
	}
	hr := h.rawState()
	if sameRaw(hr, target.rawState()) {
		return nil, nil // same as the target branch: nothing new to check
	}
	if base := diffcap.FullDiffBase(mr); base != "" {
		br, err := r.rawRepoConfig(ctx, projectID, base)
		if err != nil {
			return nil, err
		}
		if sameRaw(br, hr) {
			return nil, nil // the MR does not touch the file (the target moved on)
		}
	}
	return &h, nil
}
