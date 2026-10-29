package crossrepo

// System map, phase B: the open merge requests of the counterpart
// repositories (listed and read within the review's GitLab request budget)
// and which of them touch a contract of the reviewed merge request.

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

// apiBudget counts the GitLab requests of phase B in one review.
type apiBudget struct{ left int }

func (b *apiBudget) take() bool {
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

// counterpartMRs lists and reads the open merge requests of counterpart
// repositories within one review's GitLab request budget.
type counterpartMRs struct {
	gl       core.GitLab
	log      *slog.Logger
	lister   OpenMRs // nil: the client cannot list
	budget   apiBudget
	branches map[string]string // lower(repo) -> deployed branch
	maxMRs   int
	cache    map[string][]*openMR // lower(repo) -> open MRs (nil = not listed)
	repos    map[string]bool      // repositories listed so far
}

// list returns the open merge requests of repo heading for its deployed
// branch; false when they could not be listed (no client, caps, error).
func (cm *counterpartMRs) list(ctx context.Context, repo string) ([]*openMR, bool) {
	k := strings.ToLower(repo)
	if l, ok := cm.cache[k]; ok {
		return l, l != nil
	}
	if cm.lister == nil || (!cm.repos[k] && len(cm.repos) >= depsMaxRepos) || !cm.budget.take() {
		return nil, false
	}
	cm.repos[k] = true
	list, err := cm.lister.ListOpenMRs(ctx, repo, cm.maxMRs)
	if err != nil {
		cm.log.Warn("review: system map deps: list open merge requests", "repo", repo, "err", err)
		cm.cache[k] = nil
		return nil, false
	}
	branch := cm.branches[k]
	out := []*openMR{}
	for _, m := range list {
		if m.State != "" && m.State != "opened" {
			continue
		}
		if branch != "" && m.TargetBranch != "" && m.TargetBranch != branch {
			continue // not heading for the deployed branch
		}
		out = append(out, &openMR{repo: repo, mr: m})
	}
	cm.cache[k] = out
	return out, true
}

// diffs fetches m's changed lines once; false when the budget ran out (a
// failed fetch sets m.diffErr and still returns true).
func (cm *counterpartMRs) diffs(ctx context.Context, m *openMR) bool {
	if m.diffs {
		return true
	}
	if !cm.budget.take() {
		return false
	}
	fds, err := cm.gl.GetMRDiffs(ctx, m.mr.ProjectID, m.mr.IID)
	m.diffs = true
	if err != nil {
		cm.log.Warn("review: system map deps: merge request diffs", "repo", m.repo, "iid", m.mr.IID, "err", err)
		m.diffErr = true
		return true
	}
	m.plus, m.minus = changedLines(fds)
	return true
}

// match looks for each check in the counterpart merge requests and fills
// d.missing, d.links and d.linked. It returns every merge request that
// touches a contract, in order of discovery.
func (d *DepsRun) match(ctx context.Context, log *slog.Logger, checks []depCheck, cm *counterpartMRs) []*openMR {
	var order []*openMR
	seenMR := map[*openMR]bool{}
	for i := range checks {
		ch := &checks[i]
		var miss *depMissing
		if ch.contract.Role == sysmap.RoleConsumesNew && !ch.contract.InProviderMain {
			if !ch.contract.ProviderComplete {
				log.Info("review: system map deps: provider scan incomplete, not reporting", "contract", ch.contract.Name(), "provider", ch.contract.Provider)
				continue
			}
			miss = &depMissing{check: ch}
			d.missing = append(d.missing, miss)
		}
		for _, repo := range ch.repos {
			mrs, ok := cm.list(ctx, repo)
			if !ok {
				continue
			}
			complete := true
			for _, m := range mrs {
				if !cm.diffs(ctx, m) {
					complete = false
					break
				}
				if m.diffErr {
					complete = false // not looked at: no "none adds it"
					continue
				}
				act := matchAct(ch.re, m)
				if act == 0 {
					continue
				}
				if miss != nil && act != actRemoves {
					if len(miss.adding) < depsMaxShown {
						miss.adding = append(miss.adding, m)
					}
				} else {
					d.links[m] = append(d.links[m], depLink{check: ch, mr: m, act: act})
				}
				if !seenMR[m] {
					seenMR[m] = true
					order = append(order, m)
				}
			}
			if miss != nil && complete && strings.EqualFold(repo, ch.contract.Provider) {
				miss.checked = true
			}
		}
	}
	for _, m := range order {
		if len(d.links[m]) > 0 {
			d.linked = append(d.linked, m)
		}
	}
	return order
}

// diffDelims: characters that end a path or an event name in code.
const diffDelims = "\\s\"'`?#"

// contractRegexp matches a contract in a line of code or of an OpenAPI
// spec: the path without its API prefix, every placeholder segment as a
// placeholder of any syntax ({id}, :id, ${id}, <int:id>, %s, [id]) or a
// number; or a quoted event name. Nil when the contract is too generic.
func contractRegexp(c sysmap.DepContract) *regexp.Regexp {
	if c.Path != "" {
		key := sysmap.StripPrefix(sysmap.PathKey(c.Path))
		segs := strings.Split(strings.Trim(key, "/"), "/")
		static := false
		var parts []string
		for _, s := range segs {
			if s == "{}" {
				parts = append(parts, `(?:[^/`+diffDelims+`]*[{:<$%\[][^/`+diffDelims+`]*|[0-9]+)`)
				continue
			}
			if s == "" {
				continue
			}
			static = true
			parts = append(parts, regexp.QuoteMeta(s))
		}
		if !static {
			return nil
		}
		return regexp.MustCompile(`(?i)/` + strings.Join(parts, "/") + `/?(?:$|[^A-Za-z0-9_\-/.{$<%\[])`)
	}
	if c.Event != "" {
		q := "[\"'`]"
		return regexp.MustCompile(q + regexp.QuoteMeta(c.Event) + q)
	}
	return nil
}

// changedLines returns the added and removed lines of the code and spec
// files of a merge request diff (tests, docs and vendored files skipped),
// capped in total size.
func changedLines(fds []core.FileDiff) (plus, minus []string) {
	size := 0
	for _, fd := range fds {
		p := fd.NewPath
		if fd.DeletedFile {
			p = fd.OldPath
		}
		if !sysmap.DepsCandidate(p) {
			continue
		}
		for _, l := range strings.Split(fd.Diff, "\n") {
			if size > depsMaxDiffBytes {
				return plus, minus
			}
			switch {
			case strings.HasPrefix(l, "+++ ") || strings.HasPrefix(l, "--- "):
			case strings.HasPrefix(l, "+"):
				plus = append(plus, l[1:])
				size += len(l)
			case strings.HasPrefix(l, "-"):
				minus = append(minus, l[1:])
				size += len(l)
			}
		}
	}
	return plus, minus
}

func matchAct(re *regexp.Regexp, m *openMR) otherAct {
	in := func(ls []string) bool {
		for _, l := range ls {
			if re.MatchString(l) {
				return true
			}
		}
		return false
	}
	a, d := in(m.plus), in(m.minus)
	switch {
	case a && d:
		return actChanges
	case a:
		return actAdds
	case d:
		return actRemoves
	}
	return 0
}
