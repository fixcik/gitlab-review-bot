// Package contextrepos gives Claude read-only access to the other
// repositories of the configured GitLab groups ("context repositories"): it
// lists the groups' projects with the bot's own (Reporter) GitLab client,
// caches the list, and turns each project into a snapshot of its default
// branch via internal/repo.
//
// Nothing here can fail a review: listing and snapshot errors are logged and
// the affected repositories are skipped.
package contextrepos

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/repo"
)

// Lister lists group projects and looks up single projects (implemented
// by *gitlab.Client).
type Lister interface {
	ListGroupProjectsDetails(ctx context.Context, groupPath string) ([]gitlab.ProjectDetails, error)
	GetProjectDetails(ctx context.Context, path string) (*gitlab.ProjectDetails, error)
}

// Reasons a related repository is missing (core.ContextMissing.Reason).
const (
	ReasonNotFound  = "не найден или у бота нет доступа на чтение"
	ReasonEmpty     = "пустой репозиторий (нет ветки по умолчанию)"
	ReasonBadPath   = "недопустимый путь проекта"
	ReasonTooLarge  = "больше review.context.max_repo_mb"
	ReasonSnapshot  = "не удалось получить снимок ветки по умолчанию"
	ReasonLookupErr = "ошибка запроса к GitLab"
	ReasonExcluded  = "исключён настройкой сервиса (review.context.exclude)"
)

// lookupErrTTL: how long a failed lookup of a project outside the groups is
// remembered (a fixed permission shows up after this).
const lookupErrTTL = time.Minute

// extraKeep: projects outside the groups are kept warm this long after
// the last run that asked for them.
const extraKeep = 24 * time.Hour

// extraEntry caches a project outside gitlab.groups (context.related).
type extraEntry struct {
	d        *gitlab.ProjectDetails
	err      error
	at       time.Time // lookup time
	lastUsed time.Time
}

// Snapshotter exports default-branch snapshots (implemented by
// *repo.Manager).
type Snapshotter interface {
	Snapshots(ctx context.Context, specs []repo.SnapshotSpec, o repo.SnapshotOptions) []*repo.Snapshot
}

// Options configure a Provider.
type Options struct {
	Lister    Lister
	Snapshots Snapshotter
	// Groups are full group paths (subgroups included).
	Groups []string
	// RefreshInterval: how often the project list is re-read and each
	// repository re-fetched. Default 10m.
	RefreshInterval time.Duration
	// MaxRepos caps the repositories of one run. Default 50.
	MaxRepos int
	// MaxRepoBytes skips larger repositories. 0 = no limit.
	MaxRepoBytes int64
	// AcquireTimeout bounds how long a run waits for snapshots; repositories
	// not ready by then are skipped for that run (the background refresh in
	// Run keeps working on them). Default 3m.
	AcquireTimeout time.Duration
	Logger         *slog.Logger
	Now            func() time.Time
	// Exclude, when set, hides projects from every run (service policy,
	// review.context.exclude), related repositories included.
	Exclude func(projectPath string) bool
	// MainBranch, when set, picks the "main" (deployed) branch of a project
	// (gitlab.main_branch / main_branches); it gets the GitLab default
	// branch and returns the branch to snapshot. A configured branch that
	// does not exist falls back to the default branch.
	MainBranch func(projectPath, defaultBranch string) string
}

// warmTimeout bounds one background refresh pass.
const warmTimeout = time.Hour

// Provider implements crossrepo.ContextRepos (package review/crossrepo).
type Provider struct {
	o   Options
	log *slog.Logger

	mu       sync.Mutex // serializes list refreshes
	listedAt time.Time
	byGroup  map[string][]gitlab.ProjectDetails // last good listing per group

	extraMu sync.Mutex
	extra   map[string]*extraEntry // lower(path) -> project outside the groups
}

// New returns a Provider.
func New(o Options) *Provider {
	if o.RefreshInterval <= 0 {
		o.RefreshInterval = 10 * time.Minute
	}
	if o.MaxRepos <= 0 {
		o.MaxRepos = 50
	}
	if o.AcquireTimeout <= 0 {
		o.AcquireTimeout = 3 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Provider{o: o, log: o.Logger, byGroup: map[string][]gitlab.ProjectDetails{}, extra: map[string]*extraEntry{}}
}

// projects returns the de-duplicated, non-archived projects of all groups,
// re-listing them at most once per RefreshInterval. A group whose listing
// fails keeps its previous list.
func (p *Provider) projects(ctx context.Context) []gitlab.ProjectDetails {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listedAt.IsZero() || p.o.Now().Sub(p.listedAt) >= p.o.RefreshInterval {
		for _, g := range p.o.Groups {
			ps, err := p.o.Lister.ListGroupProjectsDetails(ctx, g)
			if err != nil {
				if ctx.Err() != nil {
					return p.mergedLocked()
				}
				p.log.Warn("context repositories: list group projects", "group", g, "err", err)
				continue
			}
			p.byGroup[g] = ps
		}
		p.listedAt = p.o.Now()
	}
	return p.mergedLocked()
}

func (p *Provider) mergedLocked() []gitlab.ProjectDetails {
	seen := map[string]bool{}
	var out []gitlab.ProjectDetails
	for _, g := range p.o.Groups {
		for _, pr := range p.byGroup[g] {
			k := strings.ToLower(pr.PathWithNamespace)
			if pr.Archived || k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, pr)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].PathWithNamespace) < strings.ToLower(out[j].PathWithNamespace)
	})
	return out
}

// Acquire returns the snapshots for one run: the related repositories
// first (beyond MaxRepos, not filtered by Exclude, looked up outside the
// groups when needed), then the group projects Exclude lets through, up to
// MaxRepos in total. missing lists related repositories that could not be
// provided and why. Call release when the run is over.
func (p *Provider) Acquire(ctx context.Context, req core.ContextRequest) ([]core.ContextDir, []core.ContextMissing, func()) {
	return p.acquire(ctx, req, p.o.AcquireTimeout, true)
}

// touch marks looked-up projects outside the groups as used (not by the
// warm pass, so they expire after extraKeep without real runs).
func (p *Provider) acquire(ctx context.Context, req core.ContextRequest, timeout time.Duration, touch bool) ([]core.ContextDir, []core.ContextMissing, func()) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pl := p.plan(ctx, req, touch)
	if len(pl.specs) == 0 {
		return nil, pl.missing, func() {}
	}
	snaps, got := p.snapshots(ctx, pl)
	missing := pl.missing
	dirs := make([]core.ContextDir, 0, len(snaps))
	for _, sp := range pl.specs { // keep the spec order: related first
		s := got[strings.ToLower(sp.Path)]
		if s == nil {
			if pl.related[strings.ToLower(sp.Path)] {
				missing = append(missing, core.ContextMissing{Path: sp.Path, Reason: ReasonSnapshot})
			}
			continue
		}
		dirs = append(dirs, core.ContextDir{HostPath: s.Dir, Name: s.Path, SHA: s.SHA, Branch: s.Branch})
	}
	var once sync.Once
	return dirs, missing, func() {
		once.Do(func() {
			for _, s := range snaps {
				s.Release()
			}
		})
	}
}

// snapshotPlan is what one run snapshots.
type snapshotPlan struct {
	specs    []repo.SnapshotSpec // related repositories first
	defaults map[string]string   // lower(path) -> GitLab default branch
	related  map[string]bool     // lower(path) of the related repositories
	missing  []core.ContextMissing
}

// plan picks the repositories of a run: the related ones, then the group
// projects up to MaxRepos.
func (p *Provider) plan(ctx context.Context, req core.ContextRequest, touch bool) *snapshotPlan {
	projects := p.projects(ctx)
	pl := &snapshotPlan{defaults: map[string]string{}, related: map[string]bool{}}
	taken := map[string]bool{}
	p.planRelated(ctx, pl, req.Related, projects, taken, touch)
	p.planGroupProjects(pl, projects, taken, req.Exclude)
	return pl
}

// planRelated adds the related repositories (beyond MaxRepos, not filtered
// by Exclude, looked up outside the groups when needed).
func (p *Provider) planRelated(ctx context.Context, pl *snapshotPlan, paths []string, projects []gitlab.ProjectDetails,
	taken map[string]bool, touch bool) {
	byPath := make(map[string]gitlab.ProjectDetails, len(projects))
	for _, pr := range projects {
		byPath[strings.ToLower(pr.PathWithNamespace)] = pr
	}
	for _, path := range paths {
		path = strings.Trim(strings.TrimSpace(path), "/")
		k := strings.ToLower(path)
		if path == "" || taken[k] {
			continue
		}
		taken[k] = true
		if p.o.Exclude != nil && p.o.Exclude(path) {
			pl.missing = append(pl.missing, core.ContextMissing{Path: path, Reason: ReasonExcluded})
			continue
		}
		pr, ok := byPath[k]
		if !ok {
			d, reason := p.lookupExtra(ctx, path, touch)
			if d == nil {
				pl.missing = append(pl.missing, core.ContextMissing{Path: path, Reason: reason})
				continue
			}
			pr = *d
		}
		if reason := p.unusable(pr); reason != "" {
			pl.missing = append(pl.missing, core.ContextMissing{Path: path, Reason: reason})
			continue
		}
		pl.related[strings.ToLower(pr.PathWithNamespace)] = true
		pl.specs = append(pl.specs, repo.SnapshotSpec{Path: pr.PathWithNamespace, DefaultBranch: p.mainBranch(pr)})
		pl.defaults[strings.ToLower(pr.PathWithNamespace)] = pr.DefaultBranch
	}
}

// planGroupProjects adds the group projects that neither the run (exclude)
// nor the service excludes, up to MaxRepos specs in total.
func (p *Provider) planGroupProjects(pl *snapshotPlan, projects []gitlab.ProjectDetails, taken map[string]bool, exclude func(string) bool) {
	for _, pr := range projects {
		path := pr.PathWithNamespace
		k := strings.ToLower(path)
		if taken[k] || (exclude != nil && exclude(path)) || (p.o.Exclude != nil && p.o.Exclude(path)) {
			continue
		}
		if reason := p.unusable(pr); reason != "" {
			switch reason {
			case ReasonTooLarge:
				p.log.Info("context repositories: repository too large, skipped", "project", path, "size_mb", pr.RepositorySize>>20)
			case ReasonBadPath:
				p.log.Warn("context repositories: unusable project path", "project", path)
			}
			continue
		}
		if len(pl.specs) >= p.o.MaxRepos {
			p.log.Warn("context repositories: max_repos reached, the rest are skipped", "max_repos", p.o.MaxRepos)
			return
		}
		pl.specs = append(pl.specs, repo.SnapshotSpec{Path: path, DefaultBranch: p.mainBranch(pr)})
		pl.defaults[k] = pr.DefaultBranch
	}
}

// snapshots exports the planned repositories. A configured main branch that
// is missing in a repository falls back to its default branch (logged), so
// the repository is not lost. got maps lower(path) to its snapshot.
func (p *Provider) snapshots(ctx context.Context, pl *snapshotPlan) (snaps []*repo.Snapshot, got map[string]*repo.Snapshot) {
	opts := repo.SnapshotOptions{RefreshInterval: p.o.RefreshInterval, MaxBytes: p.o.MaxRepoBytes}
	snaps = p.o.Snapshots.Snapshots(ctx, pl.specs, opts)
	got = make(map[string]*repo.Snapshot, len(snaps))
	for _, s := range snaps {
		got[strings.ToLower(s.Path)] = s
	}
	var retry []repo.SnapshotSpec
	for _, sp := range pl.specs {
		k := strings.ToLower(sp.Path)
		if got[k] == nil && pl.defaults[k] != "" && sp.DefaultBranch != pl.defaults[k] {
			p.log.Warn("context repositories: main branch not available, using the default branch",
				"project", sp.Path, "main_branch", sp.DefaultBranch, "default_branch", pl.defaults[k])
			retry = append(retry, repo.SnapshotSpec{Path: sp.Path, DefaultBranch: pl.defaults[k]})
		}
	}
	if len(retry) > 0 {
		more := p.o.Snapshots.Snapshots(ctx, retry, opts)
		for _, s := range more {
			got[strings.ToLower(s.Path)] = s
		}
		snaps = append(snaps, more...)
	}
	return snaps, got
}

// mainBranch is the branch to snapshot for a project.
func (p *Provider) mainBranch(pr gitlab.ProjectDetails) string {
	if p.o.MainBranch == nil {
		return pr.DefaultBranch
	}
	if b := strings.TrimSpace(p.o.MainBranch(pr.PathWithNamespace, pr.DefaultBranch)); b != "" {
		return b
	}
	return pr.DefaultBranch
}

// unusable returns why a project cannot be a context repository ("" = ok).
func (p *Provider) unusable(pr gitlab.ProjectDetails) string {
	switch {
	case pr.DefaultBranch == "":
		return ReasonEmpty
	case !repo.ValidProjectPath(pr.PathWithNamespace):
		return ReasonBadPath
	case p.o.MaxRepoBytes > 0 && pr.RepositorySize > p.o.MaxRepoBytes:
		return ReasonTooLarge
	}
	return ""
}

// lookupExtra finds a project outside the groups, cached for
// RefreshInterval (failures for lookupErrTTL).
func (p *Provider) lookupExtra(ctx context.Context, path string, touch bool) (*gitlab.ProjectDetails, string) {
	k := strings.ToLower(path)
	now := p.o.Now()
	p.extraMu.Lock()
	e := p.extra[k]
	if e != nil {
		if touch {
			e.lastUsed = now
		}
		ttl := p.o.RefreshInterval
		if e.err != nil {
			ttl = lookupErrTTL
		}
		if now.Sub(e.at) < ttl {
			d, err := e.d, e.err
			p.extraMu.Unlock()
			return d, extraReason(err)
		}
	}
	p.extraMu.Unlock()

	d, err := p.o.Lister.GetProjectDetails(ctx, path)
	if err != nil && ctx.Err() != nil {
		return nil, ReasonLookupErr // do not cache a cancelled lookup
	}
	if err != nil {
		p.log.Warn("context repositories: related project lookup failed", "project", path, "err", err)
	}
	p.extraMu.Lock()
	used := now
	if old := p.extra[k]; old != nil && !touch {
		used = old.lastUsed
	}
	p.extra[k] = &extraEntry{d: d, err: err, at: now, lastUsed: used}
	p.extraMu.Unlock()
	return d, extraReason(err)
}

func extraReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrForbidden):
		return ReasonNotFound
	}
	return ReasonLookupErr
}

// recentExtra returns the projects outside the groups asked for within
// extraKeep (kept warm by Run); older entries are forgotten.
func (p *Provider) recentExtra() []string {
	now := p.o.Now()
	p.extraMu.Lock()
	defer p.extraMu.Unlock()
	var out []string
	for k, e := range p.extra {
		if now.Sub(e.lastUsed) > extraKeep {
			delete(p.extra, k)
			continue
		}
		if e.d != nil {
			out = append(out, e.d.PathWithNamespace)
		}
	}
	sort.Strings(out)
	return out
}

// Run keeps the snapshots warm: it acquires (and immediately releases) all
// repositories at start and then every RefreshInterval, so that the first
// review after a push to another repository does not wait for the fetch.
// Blocks until ctx is done.
func (p *Provider) Run(ctx context.Context) {
	t := time.NewTicker(p.o.RefreshInterval)
	defer t.Stop()
	for {
		start := p.o.Now()
		// A long timeout: the first clone of a big repository must be able
		// to finish here even if it does not fit into a review's wait.
		dirs, _, release := p.acquire(ctx, core.ContextRequest{Related: p.recentExtra()}, warmTimeout, false)
		release()
		if ctx.Err() != nil {
			return
		}
		p.log.Info("context repositories refreshed", "repos", len(dirs), "took", p.o.Now().Sub(start).Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
