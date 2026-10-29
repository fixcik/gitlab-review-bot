package sysmap

import (
	"sort"
	"strconv"
	"strings"
)

// Impact limits.
const (
	// handlerSpan: lines a handler is assumed to span after its line when
	// the extraction gave no end line and no later entry bounds it.
	handlerSpan = 60
	// maxUsages caps the usages listed per touched contract.
	maxUsages = 50
)

// LineRange is an inclusive range of lines of the default-branch version of
// a file (the old side of the merge request diff).
type LineRange struct{ From, To int }

// EndpointRef is an HTTP endpoint the merge request changes according to
// another source (the OpenAPI changelog).
type EndpointRef struct {
	Method, Path string
}

// Usage is where another repository uses a touched contract.
type Usage struct {
	Repo     string
	File     string
	Line     int
	Protocol Protocol
	Method   string
	Path     string
	Event    string
	Raw      string
	Via      string
	// Receiver: the usage receives a webhook the reviewed repository sends
	// (otherwise it is a consumer of what the reviewed repository provides).
	Receiver bool
}

// Touched is a contract of the reviewed repository that the merge request
// touches, with its usages elsewhere.
type Touched struct {
	Protocol Protocol
	Method   string
	Path     string
	Event    string
	// File / Line: where the reviewed repository defines it ("" for an
	// endpoint known only from the OpenAPI changelog).
	File string
	Line int
	// Sender: the reviewed repository sends this webhook.
	Sender bool
	Usages []Usage
	// Possible: unresolved calls that may target the reviewed repository
	// (several providers have the same path, the reviewed one among them).
	Possible []Usage
	// More counts usages over the cap.
	More int
}

// Impact is what a merge request touches according to the map.
type Impact struct {
	// SHA / Branch: the default-branch commit the reviewed repository was
	// scanned at.
	SHA, Branch string
	Contracts   []Touched
}

// Name renders "POST /orders", "sse order.updated", "ws /socket".
func (t Touched) Name() string {
	switch {
	case t.Path != "" && (t.Protocol == HTTP || t.Protocol == Webhook):
		return strings.TrimSpace(t.Method + " " + t.Path)
	case t.Path != "" && t.Event != "":
		return t.Path + " " + t.Event
	case t.Path != "":
		return t.Path
	}
	return t.Event
}

// Impact finds the contracts of repo that the merge request touches and
// their usages in other repositories. changed holds the changed old-side
// line ranges per file (default-branch version), endpoints the endpoints
// the OpenAPI changelog reports as changed. Nil when the index is empty or
// repo is not in the map; an Impact without contracts when nothing touched
// has usages.
func (x *Index) Impact(repo string, changed map[string][]LineRange, endpoints []EndpointRef) *Impact {
	if x == nil {
		return nil
	}
	st := x.Repo(repo)
	if st == nil {
		return nil
	}
	self := lower(repo)
	imp := &Impact{SHA: st.SHA, Branch: st.Branch}

	// A contract met again (several entries, or the changelog too) merges
	// its usages into the first listing.
	seen := map[string]int{} // contract key -> index in imp.Contracts
	add := func(t Touched, key string) {
		i, ok := seen[key]
		if !ok {
			seen[key] = len(imp.Contracts)
			imp.Contracts = append(imp.Contracts, t)
			return
		}
		c := &imp.Contracts[i]
		if c.File == "" && t.File != "" { // prefer a code location
			c.File, c.Line = t.File, t.Line
		}
		c.Usages = mergeUsages(c.Usages, t.Usages)
		c.Possible = mergeUsages(c.Possible, t.Possible)
	}

	for _, i := range x.touchedEntries(self, changed) {
		e := x.Entries[i]
		t := Touched{Protocol: e.Protocol, Method: e.Method, Path: e.Path, Event: e.Event, File: e.File, Line: e.Line, Sender: e.Side == Consumes}
		if e.Side == Consumes {
			t.Usages, t.Possible = x.receivers(self, e)
		} else {
			t.Usages, t.Possible = x.consumers(self, e.Protocol, e.Method, e.key, e.Event)
		}
		add(t, contractKey(e.Side, e.Protocol, e.Method, e.key, e.Event))
	}
	for _, ep := range endpoints {
		p := NormalizePath(ep.Path)
		if p == "" {
			continue
		}
		m := NormalizeMethod(ep.Method)
		k := PathKey(p)
		t := Touched{Protocol: HTTP, Method: m, Path: p}
		t.Usages, t.Possible = x.consumers(self, HTTP, m, k, "")
		add(t, contractKey(Provides, HTTP, m, k, ""))
	}
	imp.Contracts = usedContracts(imp.Contracts)
	return imp
}

// touchedEntries returns the indexes (ascending) of self's contract entries
// whose code the changed line ranges touch. An entry without an end line is
// assumed to span up to handlerSpan lines, but not into the next entry.
func (x *Index) touchedEntries(self string, changed map[string][]LineRange) []int {
	byFile := map[string][]int{}
	for i, e := range x.Entries {
		if lower(e.Repo) == self {
			byFile[e.File] = append(byFile[e.File], i)
		}
	}
	var touched []int
	for file, ranges := range changed {
		idx := byFile[file]
		sort.Slice(idx, func(a, b int) bool { return x.Entries[idx[a]].Line < x.Entries[idx[b]].Line })
		for k, i := range idx {
			e := x.Entries[i]
			if e.Side == Consumes && e.Protocol != Webhook {
				continue // the repository's own calls: not a contract it offers
			}
			end := e.EndLine
			if end <= e.Line {
				end = e.Line + handlerSpan
				for _, j := range idx[k+1:] {
					if n := x.Entries[j].Line; n > e.Line {
						end = min(end, n-1)
						break
					}
				}
			}
			for _, r := range ranges {
				if r.From <= end && r.To >= e.Line {
					touched = append(touched, i)
					break
				}
			}
		}
	}
	sort.Ints(touched)
	return touched
}

// usedContracts keeps the contracts someone uses, with their usages sorted
// and capped at maxUsages.
func usedContracts(cs []Touched) []Touched {
	out := cs[:0]
	for _, c := range cs {
		if len(c.Usages) == 0 && len(c.Possible) == 0 {
			continue
		}
		sortUsages(c.Usages)
		sortUsages(c.Possible)
		if len(c.Usages) > maxUsages {
			c.More = len(c.Usages) - maxUsages
			c.Usages = c.Usages[:maxUsages]
		}
		if len(c.Possible) > maxUsages {
			c.Possible = c.Possible[:maxUsages]
		}
		out = append(out, c)
	}
	return out
}

func contractKey(side Side, p Protocol, method, key, event string) string {
	return string(side) + "|" + family(p) + "|" + method + "|" + StripPrefix(key) + "|" + EventKey(event)
}

// consumers lists other repositories' consumers of a contract self
// provides: resolved to self (and matching by path or event), plus
// unresolved ones whose candidates include self.
func (x *Index) consumers(self string, p Protocol, method, key, event string) (usages, possible []Usage) {
	skey := StripPrefix(key)
	for _, c := range x.Entries {
		if c.Side != Consumes || lower(c.Repo) == self || family(c.Protocol) != family(p) {
			continue
		}
		match := false
		if key != "" && c.key != "" && methodsMatch(method, c.Method) && (c.key == key || c.skey == skey) {
			match = true
		}
		if sameEvent(c.Event, event) && (key == "" || c.key == "") {
			match = true
		}
		if !match {
			continue
		}
		switch {
		case c.Status == Resolved && lower(c.Target) == self:
			usages = append(usages, usageOf(c, false))
		case c.Status == Unresolved && containsFold(c.Candidates, self):
			possible = append(possible, usageOf(c, false))
		}
	}
	return usages, possible
}

// receivers lists the endpoints / handlers that receive a webhook self
// sends (entry s).
func (x *Index) receivers(self string, s IndexedEntry) (usages, possible []Usage) {
	for _, p := range x.Entries {
		if p.Side != Provides || lower(p.Repo) == self || family(p.Protocol) != family(s.Protocol) {
			continue
		}
		byPath := s.key != "" && p.key != "" && methodsMatch(s.Method, p.Method) && (p.key == s.key || p.skey == s.skey)
		byEvent := sameEvent(s.Event, p.Event)
		switch {
		case s.Status == Resolved && lower(p.Repo) == lower(s.Target) && (byPath || byEvent):
			usages = append(usages, usageOf(p, true))
		case s.Status == Unresolved && (byPath && containsFold(s.Candidates, lower(p.Repo)) || byEvent && s.key == ""):
			possible = append(possible, usageOf(p, true))
		}
	}
	return usages, possible
}

func usageOf(e IndexedEntry, receiver bool) Usage {
	return Usage{Repo: e.Repo, File: e.File, Line: e.Line, Protocol: e.Protocol, Method: e.Method, Path: e.Path,
		Event: e.Event, Raw: e.Raw, Via: e.Via, Receiver: receiver}
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if lower(v) == s {
			return true
		}
	}
	return false
}

func mergeUsages(a, b []Usage) []Usage {
	seen := map[string]bool{}
	for _, u := range a {
		seen[usageKey(u)] = true
	}
	for _, u := range b {
		if !seen[usageKey(u)] {
			seen[usageKey(u)] = true
			a = append(a, u)
		}
	}
	return a
}

func usageKey(u Usage) string {
	return lower(u.Repo) + "|" + u.File + "|" + strconv.Itoa(u.Line) + "|" + u.Method + "|" + u.Path + "|" + u.Event
}

func sortUsages(us []Usage) {
	sort.SliceStable(us, func(i, j int) bool {
		a, b := us[i], us[j]
		if lower(a.Repo) != lower(b.Repo) {
			return lower(a.Repo) < lower(b.Repo)
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
}
