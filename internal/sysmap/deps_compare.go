package sysmap

// The comparison half of phase B: the analysed head entries on changed lines
// (and the default-branch consumers on removed lines) against the map.

import (
	"slices"
	"sort"
	"strings"
)

func inRanges(line int, rs []LineRange) bool {
	for _, r := range rs {
		if line >= r.From-depsLineSlack && line <= r.To {
			return true
		}
	}
	return false
}

// depContracts compares the analysed head entries on changed lines (and
// the default-branch consumers on removed lines) with the map.
func (x *Index) depContracts(req DepsRequest, head map[string][]Entry) []DepContract {
	var out []DepContract
	seen := map[string]bool{}
	add := func(c DepContract) {
		k := string(c.Role) + "|" + lower(c.Provider) + "|" + c.Key()
		if !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	x.newContracts(req, head, add)
	x.droppedContracts(req, head, add)
	return out
}

// newContracts reports the head entries on added lines that the default
// branch does not have: contracts provided or consumed anew.
func (x *Index) newContracts(req DepsRequest, head map[string][]Entry, add func(DepContract)) {
	self := lower(req.Repo)
	files := make([]string, 0, len(head))
	for f := range head {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, file := range files {
		ranges := req.Added[file]
		if len(ranges) == 0 {
			continue
		}
		for _, e := range head[file] {
			if !inRanges(e.Line, ranges) {
				continue
			}
			switch e.Side {
			case Provides:
				if mockPath(e.File) || e.Source == SourceOpenAPI && x.clients[self] || x.repoHas(self, Provides, e) {
					continue
				}
				add(DepContract{Role: RoleProvidesNew, Protocol: e.Protocol, Method: e.Method, Path: e.Path, Event: e.Event, File: e.File, Line: e.Line})
			case Consumes:
				if x.repoHas(self, Consumes, e) {
					continue // the default branch already uses it
				}
				target, via := x.resolveNew(req.Repo, e)
				if target == "" || lower(target) == self {
					continue
				}
				st := x.Repo(target)
				if st == nil {
					continue // a provider outside the map: nothing to compare with
				}
				add(DepContract{Role: RoleConsumesNew, Protocol: e.Protocol, Method: e.Method, Path: e.Path, Event: e.Event,
					File: e.File, Line: e.Line, Provider: st.Path, Via: via, InProviderMain: x.repoHas(lower(target), Provides, e),
					ProviderComplete: st.Status == StatusOK, ProviderBranch: st.Branch, OffTopology: x.OffTopology(self, st.Path, e.Protocol)})
			}
		}
	}
}

// OffTopology reports whether a call from -> to over p is outside the
// declared topology: from has rules, none of them to "to" over p. False
// for a repository without rules.
func (x *Index) OffTopology(from, to string, p Protocol) bool {
	has := false
	for _, r := range x.Topology {
		if lower(r.From) != lower(from) {
			continue
		}
		has = true
		if lower(r.To) == lower(to) && slices.Contains(r.Via, p) {
			return false
		}
	}
	return has
}

// droppedContracts reports the default-branch consumers on removed lines
// that the head no longer has (only for files known at the head: analysed
// or deleted).
func (x *Index) droppedContracts(req DepsRequest, head map[string][]Entry, add func(DepContract)) {
	self := lower(req.Repo)
	deleted := map[string]bool{}
	for _, d := range req.Deleted {
		deleted[d] = true
	}
	for _, e := range x.Entries {
		if lower(e.Repo) != self || e.Side != Consumes || e.Status != Resolved || lower(e.Target) == self {
			continue
		}
		ranges := req.Removed[e.File]
		if len(ranges) == 0 || !inRanges(e.Line, ranges) {
			continue
		}
		if !deleted[e.File] {
			hs, ok := head[e.File]
			if !ok || entriesHave(hs, Consumes, e.Entry) {
				continue
			}
		}
		st := x.Repo(e.Target)
		if st == nil {
			continue
		}
		add(DepContract{Role: RoleConsumesDropped, Protocol: e.Protocol, Method: e.Method, Path: e.Path, Event: e.Event,
			File: e.File, Line: e.Line, Provider: st.Path, Via: e.Via, InProviderMain: true,
			ProviderComplete: st.Status == StatusOK, ProviderBranch: st.Branch})
	}
}

// resolveNew finds the provider of a consumer entry that is not in the
// map: the matching of Build (hint, path, prefix, event), and for a path
// or event no repository provides, the base expression (ViaBase). ""
// when the provider is unknown, ambiguous or the repository itself.
func (x *Index) resolveNew(repo string, e Entry) (target, via string) {
	ie := indexEntry(e, repo)
	switch {
	case e.Path != "":
		x.matchPath(&ie)
	case e.Event != "":
		x.matchEvent(&ie)
	default:
		return "", ""
	}
	x.external(&ie)
	switch ie.Status {
	case Resolved:
		return ie.Target, ie.Via
	case Unresolved:
		if ie.Reason == ReasonNoProvider || ie.Reason == ReasonNoEventProvider {
			if t := x.baseTarget(lower(repo), e.Base); t != "" {
				return t, ViaBase
			}
		}
	}
	return "", ""
}

// baseTarget: the single repository that the default-branch calls of self
// with the same base expression resolve to ("" when there is none, more
// than one, or the repository itself is among them).
func (x *Index) baseTarget(self, base string) string {
	b := normBase(base)
	if b == "" {
		return ""
	}
	set := map[string]string{}
	for _, e := range x.Entries {
		if e.Side != Consumes || lower(e.Repo) != self || normBase(e.Base) != b {
			continue
		}
		if e.Status == Resolved || e.Status == Internal {
			set[lower(e.Target)] = e.Target
		}
	}
	if len(set) != 1 {
		return ""
	}
	for k, v := range set {
		if k == self {
			return ""
		}
		return v
	}
	return ""
}

func normBase(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}

// repoHas reports whether repo (lower case) has an entry of side that
// matches e: the same protocol family, a compatible method and the same
// path key (also without an API prefix), or for event-only entries the
// same event.
func (x *Index) repoHas(repo string, side Side, e Entry) bool {
	key := PathKey(e.Path)
	for _, c := range x.Entries {
		if lower(c.Repo) != repo || c.Side != side {
			continue
		}
		if matchesEntry(c.Entry, c.key, c.skey, e, key) {
			return true
		}
	}
	return false
}

// entriesHave is repoHas over a list of entries.
func entriesHave(list []Entry, side Side, e Entry) bool {
	key := PathKey(e.Path)
	for _, c := range list {
		if c.Side != side {
			continue
		}
		ck := PathKey(c.Path)
		if matchesEntry(c, ck, StripPrefix(ck), e, key) {
			return true
		}
	}
	return false
}

func matchesEntry(c Entry, ckey, cskey string, e Entry, key string) bool {
	if family(c.Protocol) != family(e.Protocol) {
		return false
	}
	if key != "" {
		return ckey != "" && methodsMatch(c.Method, e.Method) && (ckey == key || cskey == StripPrefix(key))
	}
	return sameEvent(c.Event, e.Event)
}
