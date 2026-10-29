package sysmap

// Phase B, the consumer side of a review: which contracts a merge request
// starts to consume (or stops consuming) and which it starts to provide,
// from its changed files at the head. The files go through the same
// prefilter, prompt, schema and model as the map; results are cached by
// content hash (shared with the default-branch files of every repository),
// and every model run counts toward DepsMaxRunsPerHour (a budget separate
// from the map's MaxRunsPerHour) and is recorded in sysmap_runs. Nothing
// here posts anything: the reviewer only renders the result into the
// summary and the prompt.

import (
	"bytes"
	"context"
	"errors"
	"path"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Defaults of DepsRequest.
const (
	DefaultDepsMaxFiles = 1000
	defaultDepsMaxRuns  = 3
	// depsLineSlack: an entry this many lines above a changed line still
	// counts as changed (a multi-line call whose URL line changed). Entries
	// that already exist on the default branch are dropped anyway.
	depsLineSlack = 2
	// maxDepsConsumers caps DepsResult.Consumers.
	maxDepsConsumers = 10
)

// DepsFile is a changed file of a merge request at its head.
type DepsFile struct {
	Path    string
	Content []byte
}

// DepsRequest asks what a merge request changes in its contracts.
type DepsRequest struct {
	Repo string
	// Files: changed files at the head (paths passing DepsCandidate), in
	// priority order. Files beyond MaxFiles code files are not analysed.
	Files []DepsFile
	// Deleted: old paths of files the merge request deletes.
	Deleted []string
	// Added: changed new-side lines per head path; Removed: changed
	// old-side (default-branch) lines per old path.
	Added, Removed map[string][]LineRange
	// Exclude: the repository's sysmap.exclude file globs.
	Exclude []string
	// MaxFiles caps the code files analysed (default 1000); MaxRuns caps the
	// model runs of this request (default 3). Runs also count toward
	// Options.MaxRunsPerHour.
	MaxFiles int
	MaxRuns  int
	// Attribute charges the runs (nil = Options.Attribute).
	Attribute func(context.Context) context.Context
}

// DepRole is what a merge request does with a contract.
type DepRole string

// Roles.
const (
	// RoleConsumesNew: a call / subscription on a changed line that the
	// repository's default branch does not have yet.
	RoleConsumesNew DepRole = "consumes-new"
	// RoleConsumesDropped: a default-branch call on a changed line that the
	// head no longer has.
	RoleConsumesDropped DepRole = "consumes-dropped"
	// RoleProvidesNew: a route / event on a changed line that the
	// repository's default branch does not provide yet.
	RoleProvidesNew DepRole = "provides-new"
)

// DepContract is one contract a merge request starts or stops using, or
// starts to provide.
type DepContract struct {
	Role     DepRole
	Protocol Protocol
	Method   string
	Path     string
	Event    string
	// File / Line in the reviewed repository (head for new entries, the
	// default branch for dropped ones).
	File string
	Line int
	// Consumer roles: the provider repository and how it was found (Via*).
	Provider string
	Via      string
	// InProviderMain: the provider's default branch provides the contract.
	InProviderMain bool
	// ProviderComplete: the provider's last scan is complete (status ok),
	// so a missing contract is not merely waiting for extraction.
	ProviderComplete bool
	// ProviderBranch: the provider's default branch.
	ProviderBranch string
	// OffTopology (new consumers): the reviewed repository has
	// review.sysmap.topology rules, none of them for this provider and
	// protocol ("вне заявленной архитектуры").
	OffTopology bool
}

// Name renders "GET /orders/{id}", "sse order.updated".
func (c DepContract) Name() string {
	return Touched{Protocol: c.Protocol, Method: c.Method, Path: c.Path, Event: c.Event}.Name()
}

// Key identifies the contract regardless of placeholder names and API
// prefixes: family|method|stripped path key|event.
func (c DepContract) Key() string {
	k := ""
	if c.Path != "" {
		k = StripPrefix(PathKey(c.Path))
	}
	return family(c.Protocol) + "|" + c.Method + "|" + k + "|" + EventKey(c.Event)
}

// DepsResult is the answer to a DepsRequest.
type DepsResult struct {
	Contracts []DepContract
	// Consumers: repositories whose default branch already calls the
	// reviewed repository (where a new contract is likely to be used).
	Consumers []string
	// Branches: the default branch of every repository in the map, by
	// lower-case path (open merge requests elsewhere are not deployed).
	Branches map[string]string
	// Files analysed; Cached of them came from the hash cache, Extracted
	// from model runs (Runs runs).
	Files, Cached, Extracted, Runs int
	// Skipped: candidate files not analysed, with the reason (budget,
	// run error, rate limit, cap).
	Skipped    int
	SkipReason string
}

// DepsCandidate reports whether a changed path may hold contracts worth
// extracting: a code file outside dependency, test and generated
// directories, or a possible OpenAPI spec. Cheap, by name only.
func DepsCandidate(p string) bool {
	segs := strings.Split(p, "/")
	for _, s := range segs[:len(segs)-1] {
		if strings.HasPrefix(s, ".") || skipDirs[strings.ToLower(s)] {
			return false
		}
	}
	name := segs[len(segs)-1]
	ext := strings.ToLower(path.Ext(name))
	switch {
	case specExt(ext):
		return specCandidateName(p)
	case codeExts[ext]:
		return !skipFileRe.MatchString(name) && !inNoCodeDir(p)
	}
	return false
}

// Dependencies analyses the changed files of a merge request: it extracts
// their contracts (cache first, then the model within the budget) and
// compares the entries on changed lines with the map. Nil when the map is
// empty or the repository is not in it. Files that could not be analysed
// are simply left out (SkipReason says why); an error is returned only
// when nothing could be done.
func (m *Map) Dependencies(ctx context.Context, req DepsRequest) (*DepsResult, error) {
	x := m.Index()
	if x == nil || x.Repo(req.Repo) == nil {
		return nil, nil
	}
	if req.MaxFiles <= 0 {
		req.MaxFiles = DefaultDepsMaxFiles
	}
	if req.MaxRuns <= 0 {
		req.MaxRuns = defaultDepsMaxRuns
	}
	res := &DepsResult{Branches: map[string]string{}}
	for _, st := range x.Repos {
		res.Branches[lower(st.Path)] = st.Branch
	}
	head := map[string][]Entry{} // analysed head files -> entries
	code := m.depsFiles(req, head, res)
	if err := m.extractDeps(ctx, req, code, head, res); err != nil && res.Files == 0 {
		return res, err
	}
	res.Contracts = x.depContracts(req, head)
	self := lower(req.Repo)
	for _, ed := range x.Edges {
		if lower(ed.To) == self && lower(ed.From) != self && len(res.Consumers) < maxDepsConsumers {
			res.Consumers = append(res.Consumers, ed.From)
		}
	}
	return res, nil
}

// depsFiles sorts the changed files: OpenAPI specs are parsed and files
// without any sign of a contract are settled right away (into head); the
// code files left for extraction are returned (at most req.MaxFiles).
func (m *Map) depsFiles(req DepsRequest, head map[string][]Entry, res *DepsResult) []fileContent {
	var code []fileContent
	for _, f := range req.Files {
		if !DepsCandidate(f.Path) || excluded(req.Exclude, f.Path) {
			continue
		}
		if specExt(strings.ToLower(path.Ext(f.Path))) {
			if len(f.Content) > maxSpecBytes || !compat.LooksLikeOpenAPI(f.Content) {
				continue
			}
			es, err := ParseSpec(f.Path, f.Content)
			if err != nil {
				continue
			}
			head[f.Path] = es
			res.Files++
			continue
		}
		if int64(len(f.Content)) > m.o.MaxFileBytes || bytes.IndexByte(f.Content, 0) >= 0 {
			continue
		}
		if !hintRe.Match(f.Content) {
			// No sign of a contract: nothing provided or consumed.
			head[f.Path] = []Entry{}
			res.Files++
			continue
		}
		if len(code) >= req.MaxFiles {
			res.Skipped++
			res.SkipReason = "files cap"
			continue
		}
		text, n := numbered(string(f.Content))
		code = append(code, fileContent{candidate: candidate{Path: f.Path, Hash: hashOf(f.Content), Size: int64(len(f.Content))}, text: text, lines: n})
	}
	return code
}

// extractDeps fills head with the entries of the code files: cached
// results by content hash first, then model runs within the budget.
func (m *Map) extractDeps(ctx context.Context, req DepsRequest, code []fileContent, head map[string][]Entry, res *DepsResult) error {
	if len(code) == 0 {
		return nil
	}
	hashes := make([]string, 0, len(code))
	for _, f := range code {
		hashes = append(hashes, f.Hash)
	}
	cached, err := m.o.Store.SysmapEntriesByHash(ctx, CodeVersion, hashes)
	if err != nil {
		m.log.Warn("sysmap: deps cache lookup", "repo", req.Repo, "err", err)
		cached = nil
	}
	var todo []fileContent
	for _, f := range code {
		if rec, ok := cached[f.Hash]; ok {
			head[f.Path] = withFile(rec.Entries, f.Path)
			res.Files++
			res.Cached++
			continue
		}
		todo = append(todo, f)
	}
	if len(todo) == 0 {
		return nil
	}
	left, err := m.runsLeft(ctx, RunKindDeps)
	if err != nil {
		res.Skipped += len(todo)
		res.SkipReason = "budget unknown"
		return err
	}
	allowed := min(left, req.MaxRuns)
	if allowed <= 0 {
		res.Skipped += len(todo)
		res.SkipReason = "budget exhausted"
		if m.runLimit(RunKindDeps) <= 0 {
			res.SkipReason = "code extraction off"
		}
		m.log.Info("sysmap: deps: extraction budget exhausted, skipping files", "repo", req.Repo, "files", len(todo),
			"deps_max_runs_per_hour", m.runLimit(RunKindDeps))
		return nil
	}
	var lastErr error
	for _, b := range batches(todo, m.o.MaxBatchChars, m.o.MaxBatchFiles) {
		if res.Runs >= allowed || ctx.Err() != nil {
			res.Skipped += len(b)
			if res.SkipReason == "" {
				res.SkipReason = "budget exhausted"
			}
			continue
		}
		res.Runs++
		recs, err := m.extractWith(ctx, req.Repo, whereMR, req.Attribute, b)
		var rl *core.ErrClaudeRateLimited
		switch {
		case errors.As(err, &rl):
			m.log.Warn("sysmap: deps: claude rate limited", "repo", req.Repo, "err", err)
			res.Skipped += len(b)
			res.SkipReason = "rate limited"
			allowed = 0
			lastErr = err
			continue
		case err != nil:
			m.log.Warn("sysmap: deps: extraction run failed", "repo", req.Repo, "files", len(b), "err", err)
			res.Skipped += len(b)
			res.SkipReason = "run failed"
			lastErr = err
			continue
		}
		if err := m.o.Store.SaveSysmapCache(ctx, recs); err != nil {
			m.log.Warn("sysmap: deps: save cache", "repo", req.Repo, "err", err)
		}
		for _, r := range recs {
			head[r.Path] = withFile(r.Entries, r.Path)
			res.Files++
			res.Extracted++
		}
	}
	return lastErr
}

// withFile returns a copy of entries attributed to file.
func withFile(entries []Entry, file string) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		e.File = file
		out = append(out, e)
	}
	return out
}
