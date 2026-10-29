package crossrepo

// System map, phase B (internal/sysmap/deps.go): what the merge request
// starts or stops consuming in other repositories and what it starts to
// provide, whether the provider's default branch (= deployed) already has
// a newly used contract, and open merge requests in the counterpart
// repositories that touch the same contracts (with the Jira story or epic
// they share). Deterministic facts only, rendered into ONE summary block
// and the system prompt. Phase B never posts a thread, never adds a
// blocking marker and never changes the merge gate; Claude is told that
// findings based on it are at most "major". Nothing here fails a review.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// SystemMapDeps is the consumer-side analysis of the map (implemented by
// *sysmap.Map; looked up on the SystemMap by a type assertion).
type SystemMapDeps interface {
	Dependencies(ctx context.Context, req sysmap.DepsRequest) (*sysmap.DepsResult, error)
}

// OpenMRs lists a project's open merge requests (implemented by
// *gitlab.Client; looked up on the GitLab client with core.GitLabAs).
// Read-only.
type OpenMRs interface {
	ListOpenMRs(ctx context.Context, projectPath string, limit int) ([]core.MergeRequest, error)
}

const (
	// depsCollapseLines: more list lines than this -> collapsed <details>.
	depsCollapseLines = 4
	// depsMaxRepos caps the counterpart repositories looked at.
	depsMaxRepos = 8
	// depsMaxJira caps the Jira lookups for linked merge requests.
	depsMaxJira = 5
	// depsMaxShown caps the merge requests listed per missing contract.
	depsMaxShown = 3
	// depsReadFactor: changed candidate files read from git, as a multiple
	// of deps_max_files (the rest is never read).
	depsReadFactor = 3
	// depsMaxDiffBytes caps the diff text of one counterpart merge request.
	depsMaxDiffBytes = 2 << 20
	// roleProvidesChanged: a contract of the reviewed repository the merge
	// request changes or removes (phase A impact), local to this package.
	roleProvidesChanged sysmap.DepRole = "provides-changed"
)

// otherAct is what a counterpart merge request does with a contract.
type otherAct int

const (
	actAdds    otherAct = iota + 1 // only on added lines
	actRemoves                     // only on removed lines
	actChanges                     // on both
)

// depCheck is one contract of the reviewed merge request with the
// repositories where an open merge request may touch it.
type depCheck struct {
	contract sysmap.DepContract
	repos    []string
	re       *regexp.Regexp // matches the contract in diff lines
}

// openMR is an open merge request of a counterpart repository.
type openMR struct {
	repo        string
	mr          core.MergeRequest
	plus        []string // added lines of code/spec files
	minus       []string // removed lines
	diffs       bool     // diffs were fetched
	diffErr     bool     // fetching them failed: plus/minus unknown
	share       string   // shared story / epic, rendered
	sharePrompt string
}

// depLink: a counterpart merge request touches a contract of the reviewed one.
type depLink struct {
	check *depCheck
	mr    *openMR
	act   otherAct
}

// depMissing: the merge request starts using a contract its provider's
// default branch does not have.
type depMissing struct {
	check   *depCheck
	adding  []*openMR // open MRs of the provider adding it
	checked bool      // the provider's open MRs were looked at
}

// DepsRun is phase B's answer for one review.
type DepsRun struct {
	missing []*depMissing
	linked  []*openMR // merge requests with links, in order
	links   map[*openMR][]depLink
	// budgetHit: deps_max_api_calls ran out.
	budgetHit bool
	// offTopology: new calls outside review.sysmap.topology (one summary
	// line, not a fact for routing or the prompt).
	offTopology []sysmap.DepContract
	Block       string // summary block with markers
	System      string // system prompt section
}

// HasFacts reports whether phase B found a contract missing from its
// provider or a linked merge request (false on nil).
func (d *DepsRun) HasFacts() bool {
	return d != nil && (len(d.missing) > 0 || len(d.linked) > 0)
}

// CollectSysmapDeps runs phase B. diff is the full MR diff (merge base ..
// head). Nil when off, when the repository is not in the map, on any
// error, or when there is nothing to say.
func (xr *Feature) CollectSysmapDeps(ctx context.Context, job core.Job, co core.Checkout, mr *core.MergeRequest, eff config.Effective,
	diff string, sm *SysmapRun, tc *task.Context, rc *RunContext) (out *DepsRun) {
	ctx, tsp := tracing.Start(ctx, "review.sysmap.deps")
	defer tsp.End()
	c := xr.env.Cfg.Review.Sysmap
	if xr.sysmap == nil || !xr.env.Cfg.SysmapEnabled() || !c.Deps {
		return nil
	}
	dm, ok := xr.sysmap.(SystemMapDeps)
	if !ok {
		return nil
	}
	log := xr.env.Log.With("mr", job.MRKey())
	defer func() {
		if p := recover(); p != nil {
			log.Error("review: system map deps panicked, continuing without them", "panic", p)
			jobctx.BotProblem(ctx, problems.SysmapFailed, "зависимости по карте системы: внутренняя ошибка", fmt.Sprint(p))
			out = nil
		}
	}()
	pd := parseDiffLines(diff)
	files := depsFiles(ctx, co, mr.DiffRefs.HeadSHA, pd.changed, eff.SysmapExclude, depsReadFactor*max(c.DepsMaxFiles, 1))
	deps, err := dm.Dependencies(ctx, sysmap.DepsRequest{
		Repo: job.ProjectPath, Files: files, Deleted: pd.deleted, Added: pd.added, Removed: pd.removed,
		Exclude: eff.SysmapExclude, MaxFiles: c.DepsMaxFiles, MaxRuns: c.DepsMaxRuns,
		Attribute: func(ctx context.Context) context.Context {
			// A map run, charged to the merge request but not to a user.
			return meter.With(ctx, meter.Attribution{Kind: meter.KindMap, ProjectID: job.ProjectID, ProjectPath: job.ProjectPath,
				MRIID: job.MRIID, MRTitle: mr.Title})
		},
	})
	if err != nil {
		log.Warn("review: system map deps", "err", err)
		jobctx.BotProblem(ctx, problems.SysmapFailed, "зависимости по карте системы: ошибка", err.Error())
	}
	if deps == nil {
		return nil
	}
	if deps.SkipReason != "" {
		log.Info("review: system map deps: files not analysed", "skipped", deps.Skipped, "reason", deps.SkipReason)
		if deps.SkipReason != "code extraction off" { // switched off by the operator: not a problem
			jobctx.BotProblem(ctx, problems.SysmapFailed, "зависимости по карте системы: файлы не проанализированы ("+deps.SkipReason+")",
				fmt.Sprintf("не проанализировано файлов: %d", deps.Skipped))
		}
	}
	cm := &counterpartMRs{gl: xr.env.GitLab, log: log, budget: apiBudget{left: c.DepsMaxAPICalls}, branches: deps.Branches, maxMRs: c.DepsMaxMRs,
		cache: map[string][]*openMR{}, repos: map[string]bool{}}
	cm.lister, _ = core.GitLabAs[OpenMRs](xr.env.GitLab)
	d := &DepsRun{links: map[*openMR][]depLink{}}
	order := d.match(ctx, log, buildDepChecks(deps, sm, job.ProjectPath), cm)
	d.budgetHit = cm.budget.left <= 0
	for _, c := range deps.Contracts {
		if c.Role == sysmap.RoleConsumesNew && c.OffTopology {
			d.offTopology = append(d.offTopology, c)
		}
	}
	xr.depsShared(ctx, log, order, tc)
	d.Block = rc.Link(xr.renderDepsBlock(d, job.ProjectPath))
	d.System = renderDepsPrompt(d, job.ProjectPath, rc, xr.env.DockerSandbox(), xr.env.Cfg.Claude.SearchModel != "")
	log.Info("review: system map deps", "contracts", len(deps.Contracts), "missing", len(d.missing), "linked", len(d.linked),
		"files", deps.Files, "cached", deps.Cached, "runs", deps.Runs, "api_calls", c.DepsMaxAPICalls-cm.budget.left)
	if d.Block == "" && d.System == "" {
		return nil
	}
	return d
}

// depsFiles reads the changed candidate files (code and specs, not
// excluded) at head, at most limit of them; unreadable ones are skipped.
func depsFiles(ctx context.Context, co core.Checkout, head string, changed, exclude []string, limit int) []sysmap.DepsFile {
	var files []sysmap.DepsFile
	for _, p := range changed {
		if !sysmap.DepsCandidate(p) || diffcap.IgnoredPath(p, exclude) {
			continue
		}
		if len(files) >= limit {
			break
		}
		b, err := co.ShowFile(ctx, head, p)
		if err != nil {
			continue
		}
		files = append(files, sysmap.DepsFile{Path: p, Content: b})
	}
	return files
}

// buildDepChecks lists the contracts to look for in counterpart merge
// requests, most important first: newly used contracts missing from the
// provider, other newly used and dropped ones, changed and new provided
// ones.
func buildDepChecks(deps *sysmap.DepsResult, sm *SysmapRun, self string) []depCheck {
	var out []depCheck
	seen := map[string]bool{}
	add := func(c sysmap.DepContract, repos []string) {
		re := contractRegexp(c)
		if re == nil {
			return
		}
		k := string(c.Role) + "|" + strings.ToLower(c.Provider) + "|" + c.Key()
		if seen[k] {
			return
		}
		seen[k] = true
		var rs []string
		for _, r := range repos {
			if r != "" && !strings.EqualFold(r, self) && !containsFold(rs, r) {
				rs = append(rs, r)
			}
		}
		out = append(out, depCheck{contract: c, repos: rs, re: re})
	}
	rank := func(c sysmap.DepContract) int {
		switch {
		case c.Role == sysmap.RoleConsumesNew && !c.InProviderMain:
			return 0
		case c.Role == sysmap.RoleConsumesNew:
			return 1
		case c.Role == sysmap.RoleConsumesDropped:
			return 2
		}
		return 4
	}
	cs := append([]sysmap.DepContract(nil), deps.Contracts...)
	sort.SliceStable(cs, func(i, j int) bool { return rank(cs[i]) < rank(cs[j]) })
	for _, c := range cs {
		if c.Role == sysmap.RoleConsumesNew || c.Role == sysmap.RoleConsumesDropped {
			add(c, []string{c.Provider})
		}
	}
	if sm != nil && sm.Impact != nil {
		for _, t := range sm.Impact.Contracts {
			if t.Sender {
				continue // the counterpart is a receiver: no clear merge order
			}
			var repos []string
			for _, u := range t.Usages {
				repos = append(repos, u.Repo)
			}
			add(sysmap.DepContract{Role: roleProvidesChanged, Protocol: t.Protocol, Method: t.Method, Path: t.Path, Event: t.Event,
				File: t.File, Line: t.Line}, repos)
		}
	}
	for _, c := range cs {
		if c.Role == sysmap.RoleProvidesNew {
			add(c, deps.Consumers)
		}
	}
	return out
}

// containsFold reports whether list holds s, ignoring case.
func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
