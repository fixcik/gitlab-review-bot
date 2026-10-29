package sysmap

import "sort"

// Relations link the entries of an Index to each other for the /map page:
// which provider entries a call lands on, who calls an endpoint, which
// calls are linked to a repository that has no such endpoint. Indices are
// positions in Index.Entries. The rules are those of Build and Impact:
// matchesEntry (protocol family, compatible method, path key as written or
// without the /api, /api/vN, /vN prefix, the event name of event-only
// entries), plus an event match when one side has no path (consumers).
type Relations struct {
	// Lands[i]: the provider entries consumer i lands on, among the entries
	// of its target (resolved / internal) or of its candidates
	// (unresolved). Nil for providers.
	Lands [][]int
	// NoEndpoint[i]: consumer i is resolved, but its target provides no
	// matching entry (a hint to a repository without such a route, or to
	// one outside the map).
	NoEndpoint []bool
	// Endpoints group the provider entries of one contract of one
	// repository (the same endpoint from an OpenAPI spec and from code),
	// in the order of their first entry.
	Endpoints []EndpointGroup
	// EndpointOf[i]: the index in Endpoints of provider i, -1 for
	// consumers.
	EndpointOf []int
}

// EndpointGroup is one endpoint of a repository.
type EndpointGroup struct {
	Repo string
	// Entries: the provider entries describing it (first = the one shown).
	Entries []int
	// Callers: the resolved consumers of other repositories that land on
	// it, in entry order.
	Callers []int
}

// Relations returns the relations of the index (computed by Build; empty
// for a nil index).
func (x *Index) Relations() *Relations {
	if x == nil {
		return &Relations{}
	}
	if x.rel == nil {
		return x.buildRelations()
	}
	return x.rel
}

// landsOn reports whether consumer c reaches provider p.
func landsOn(c, p IndexedEntry) bool {
	if matchesEntry(p.Entry, p.key, p.skey, c.Entry, c.key) {
		return true
	}
	// A call with a path and an event (`new WebSocket(".../ws")` +
	// `msg.event === "user.created"`) also reaches the event-only entry
	// that emits that event (as in Index.consumers).
	return c.key != "" && p.key == "" && family(c.Protocol) == family(p.Protocol) && sameEvent(c.Event, p.Event)
}

func (x *Index) buildRelations() *Relations {
	n := len(x.Entries)
	rel := &Relations{Lands: make([][]int, n), NoEndpoint: make([]bool, n), EndpointOf: make([]int, n)}
	byRepo := map[string][]int{}
	groups := map[string]int{}
	for i, e := range x.Entries {
		rel.EndpointOf[i] = -1
		if e.Side != Provides {
			continue
		}
		r := lower(e.Repo)
		byRepo[r] = append(byRepo[r], i)
		k := r + "|" + string(e.Protocol) + "|" + contractKey(e.Side, e.Protocol, e.Method, e.key, e.Event)
		g, ok := groups[k]
		if !ok {
			g = len(rel.Endpoints)
			groups[k] = g
			rel.Endpoints = append(rel.Endpoints, EndpointGroup{Repo: e.Repo})
		}
		rel.Endpoints[g].Entries = append(rel.Endpoints[g].Entries, i)
		rel.EndpointOf[i] = g
	}
	for i, c := range x.Entries {
		if c.Side != Consumes {
			continue
		}
		targets := c.Candidates
		switch {
		case c.Status == Resolved || c.Status == Internal || c.Status == Topology && c.Via == ViaTopology:
			targets = []string{c.Target}
		case c.Status == Topology:
			targets = nil // the rule links repositories, not an endpoint
		}
		for _, t := range targets {
			for _, pi := range byRepo[lower(t)] {
				if landsOn(c, x.Entries[pi]) {
					rel.Lands[i] = append(rel.Lands[i], pi)
				}
			}
		}
		sort.Ints(rel.Lands[i])
		if c.Status != Resolved && c.Via != ViaTopology {
			continue
		}
		rel.NoEndpoint[i] = c.Status == Resolved && len(rel.Lands[i]) == 0
		seen := map[int]bool{}
		for _, pi := range rel.Lands[i] {
			if g := rel.EndpointOf[pi]; !seen[g] {
				seen[g] = true
				rel.Endpoints[g].Callers = append(rel.Endpoints[g].Callers, i)
			}
		}
	}
	return rel
}
