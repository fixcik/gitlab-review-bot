package sysmap

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/fixcik/gitlab-review-bot/internal/config"
)

// Resolution statuses of a consumer entry.
const (
	Resolved   = "resolved"
	Unresolved = "unresolved"
	// Internal: the call targets the consuming repository itself.
	Internal = "internal"
	// External: the call goes to a third-party system outside the
	// repositories (IndexedEntry.ExternalHost); not a gap of the map.
	External = "external"
	// Topology: linked by a review.sysmap.topology rule after nothing in
	// the code matched (Target set; Via ViaTopology or ViaTopologyEdge).
	// Neither unresolved nor found in code.
	Topology = "topology"
)

// ExternalRule is one review.sysmap.externals entry: calls from the files
// of Repo matching Paths go to the external system Name (Host optional).
type ExternalRule struct {
	Name, Repo, Host string
	Paths            []string
}

// ExternalsFromConfig converts review.sysmap.externals (validated).
func ExternalsFromConfig(rules []config.SysmapExternal) []ExternalRule {
	var out []ExternalRule
	for _, r := range rules {
		n := r.Normalized()
		out = append(out, ExternalRule{Name: n.Name, Repo: n.Repo, Host: n.Host, Paths: n.Paths})
	}
	return out
}

// thirdPartyNames: words in a file path that name a third-party system;
// the topology does not guess for calls from such files (a gateway to
// Bitrix24, Jira, …): only review.sysmap.externals decides.
var thirdPartyNames = []string{"bitrix", "confluence", "jira", "atlassian", "gitlab", "github", "bitbucket", "slack", "telegram", "sentry", "grafana", "docling"}

// thirdPartyPath reports a file path naming a third-party system.
func thirdPartyPath(p string) bool {
	p = strings.ToLower(p)
	for _, n := range thirdPartyNames {
		if strings.Contains(p, n) {
			return true
		}
	}
	return false
}

// configuredExternal returns the review.sysmap.externals rule of a
// consumer's file (nil when none).
func (x *Index) configuredExternal(e *IndexedEntry) *ExternalRule {
	for i := range x.externals {
		r := &x.externals[i]
		if lower(r.Repo) != lower(e.Repo) {
			continue
		}
		for _, g := range r.Paths {
			if ok, err := doublestar.Match(g, e.File); err == nil && ok {
				return r
			}
		}
	}
	return nil
}

// externalByConfig applies a review.sysmap.externals rule (rule 0 of
// Build): an exact code provider (one repository, or the caller's own
// route) still wins; everything else is a call of the external system.
func (x *Index) externalByConfig(e *IndexedEntry, match func(*IndexedEntry)) bool {
	r := x.configuredExternal(e)
	if r == nil {
		return false
	}
	match(e)
	if e.Status == Resolved || e.Status == Internal {
		return true
	}
	e.Status, e.Target, e.Via, e.Reason, e.Candidates = External, "", "", "", nil
	e.ExternalService, e.ExternalHost = r.Name, r.Host
	return true
}

// TopologyRule is one review.sysmap.topology rule: From calls To over
// the Via protocols.
type TopologyRule struct {
	From, To string
	Via      []Protocol
}

// How a consumer was matched to its provider (IndexedEntry.Via).
const (
	ViaPath   = "path"   // same method + normalized path, one provider repo
	ViaPrefix = "prefix" // the same after dropping /api, /api/vN, /vN
	ViaHint   = "hint"   // review.sysmap.hints
	ViaPair   = "pair"   // event name inside an already connected pair
	ViaEvent  = "event"  // a specific event name only one repository emits
	// ViaEventSuffix: a webhook sent without a path whose event name ends
	// the only such event of a repository already connected by a path call
	// ("usage" -> "llm.usage").
	ViaEventSuffix = "event-suffix"
	// ViaTopology: a topology rule picked its target among the call's
	// candidates (the call lands on that endpoint); ViaTopologyEdge: the
	// rule links the repositories only (no endpoint known).
	ViaTopology     = "topology"
	ViaTopologyEdge = "topology-edge"
	// ViaBase: a merge request's call whose path no repository provides,
	// attributed by its base expression, which the repository's other calls
	// use for exactly one provider (deps.go; never set by Build).
	ViaBase = "base"
)

// Reasons of unresolved consumers that deps.go looks at.
const (
	ReasonNoProvider      = "нет провайдера с таким путём"
	ReasonNoEventProvider = "нет провайдера события"
)

// IndexedEntry is an Entry of a repository with its matching result.
type IndexedEntry struct {
	Entry
	Repo string `json:"repo"`
	// Consumers only.
	Status     string   `json:"status,omitempty"`
	Target     string   `json:"target,omitempty"`
	Via        string   `json:"via,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Candidates []string `json:"candidates,omitempty"`

	key, skey string // PathKey and StripPrefix(PathKey)
	// urlHost: the public host of a literal URL in Raw or Base (consumers
	// without the model's mark only), see external.
	urlHost string
}

// Edge is a consumer -> provider connection with counts per protocol.
type Edge struct {
	From   string           `json:"from"`
	To     string           `json:"to"`
	Counts map[Protocol]int `json:"counts"`
	Total  int              `json:"total"`
	// RuleCounts / RuleTotal: calls linked by a topology rule (not in
	// Counts / Total, which are calls matched in code).
	RuleCounts map[Protocol]int `json:"rule_counts,omitempty"`
	RuleTotal  int              `json:"rule_total,omitempty"`
}

// Index is an immutable snapshot of the map with every consumer matched.
type Index struct {
	BuiltAt time.Time
	Repos   []RepoState
	Entries []IndexedEntry
	Edges   []Edge
	Hints   map[string]string
	// FileErrors counts files whose extraction answer was unusable.
	FileErrors map[string]int
	// Topology: the configured rules; TopologyHits[i]: calls rule i linked.
	Topology     []TopologyRule
	TopologyHits []int
	// externals: review.sysmap.externals.
	externals []ExternalRule

	// Matching tables of Build, kept to match entries that are not in the
	// map (a merge request's head, see deps.go).
	exact, stripped map[string][]provider      // family|key -> providers
	events          map[string][]string        // protocol|lower(event) -> provider repos
	pairs           map[string]map[string]bool // family|lower(consumer) -> lower(providers)
	connected       map[string]map[string]bool // lower(repo) -> repos linked by a path call, either way
	hintKeys        []string
	// clients: repositories with calls and no route in code (lower case):
	// their spec files are not their API (dropClientSpecs).
	clients map[string]bool
	// ownHosts: URL hosts of matched consumers (never external).
	ownHosts map[string]bool
	// ownDomain: the organization's domain (OwnDomain of gitlab.url); its
	// hosts are never external.
	ownDomain string
	// rel: the relations of the entries (relations.go), built once.
	rel *Relations
}

// provider is one provider of a path key; spec: described by an
// OpenAPI/Swagger file, not found in code.
type provider struct {
	repo, method string
	spec         bool
}

// family groups protocols that match each other by path: a webhook
// receiver endpoint is an HTTP endpoint.
func family(p Protocol) string {
	switch p {
	case HTTP, Webhook:
		return "http"
	}
	return string(p)
}

func methodsMatch(a, b string) bool { return a == "" || b == "" || a == b }

func lower(s string) string { return strings.ToLower(strings.Trim(strings.TrimSpace(s), "/")) }

// Build matches every consumer to its provider. Matching is deterministic:
//
//  1. a hint (base expression or env var name -> repository) wins;
//  2. otherwise the providers with the same protocol family, a compatible
//     method and the same path key; when there are none, the same after
//     dropping a common prefix (/api, /api/vN, /vN) on either side;
//  3. exactly one provider repository -> resolved (the consumer's own
//     repository -> internal); none or several -> unresolved, with the
//     candidates. Of several, the only one that serves the route in code
//     wins over those that only describe it in an OpenAPI/Swagger file;
//  4. event-only consumers (ws/sse/webhook events) match a provider of the
//     same event among the repositories the consumer is already connected
//     to by path; without such a pair, a specific event name (with a
//     separator such as "." or ":") that exactly one repository provides;
//  5. a consumer the model marked as a call to a third-party system
//     (ExternalHost) is external right after the hint check, without path
//     or event matching; a consumer left unresolved whose call has a
//     literal URL with a public host (records extracted before the mark
//     existed) is external too, unless a matched consumer's URL has the
//     same host (the team's own domain). Hosts under the organization's
//     domain (build: OwnDomain of gitlab.url) are never external. External
//     calls make no edges.
func Build(repos []RepoState, files map[string][]FileRecord, hints map[string]string, now time.Time) *Index {
	return build(repos, files, hints, "", nil, nil, now)
}

// BuildTopology is Build with review.sysmap.topology rules.
func BuildTopology(repos []RepoState, files map[string][]FileRecord, hints map[string]string, topology []TopologyRule, now time.Time) *Index {
	return build(repos, files, hints, "", topology, nil, now)
}

// BuildConfigured is Build with topology rules and external systems.
func BuildConfigured(repos []RepoState, files map[string][]FileRecord, hints map[string]string, topology []TopologyRule, externals []ExternalRule, now time.Time) *Index {
	return build(repos, files, hints, "", topology, externals, now)
}

// build is Build for an organization domain (OwnDomain of gitlab.url,
// "" = none: calls to its hosts are never external) and topology rules.
func build(repos []RepoState, files map[string][]FileRecord, hints map[string]string, ownDomain string, topology []TopologyRule, externals []ExternalRule, now time.Time) *Index {
	x := &Index{BuiltAt: now, Repos: append([]RepoState(nil), repos...), Hints: hints, FileErrors: map[string]int{}, ownDomain: ownDomain,
		Topology: topology, TopologyHits: make([]int, len(topology)), externals: externals}
	sort.Slice(x.Repos, func(i, j int) bool { return lower(x.Repos[i].Path) < lower(x.Repos[j].Path) })
	x.addEntries(files)
	x.dropClientSpecs()
	x.buildProviderTables()
	x.hintKeys = sortedHintKeys(hints)

	// Pass 1: consumers with a path.
	for i := range x.Entries {
		if e := &x.Entries[i]; e.Side == Consumes && e.key != "" {
			x.matchPath(e)
		}
	}
	x.buildPairs()
	// Pass 2: event-only consumers.
	for i := range x.Entries {
		if e := &x.Entries[i]; e.Side == Consumes && e.key == "" && e.Event != "" {
			x.matchEvent(e)
		}
	}
	// Pass 3: unresolved calls to a public host no matched call uses.
	x.buildOwnHosts()
	for i := range x.Entries {
		x.external(&x.Entries[i])
	}
	// Pass 4: what is still unresolved, by the declared topology.
	for i := range x.Entries {
		x.byTopology(&x.Entries[i])
	}
	x.buildEdges()
	x.rel = x.buildRelations()
	return x
}

// indexEntry is e of repo with its path keys, not matched yet.
func indexEntry(e Entry, repo string) IndexedEntry {
	ie := IndexedEntry{Entry: e, Repo: repo}
	if e.Side == Consumes && e.ExternalHost == "" {
		ie.urlHost = urlHost(e.Raw, e.Base)
	}
	if e.Path != "" {
		ie.key = PathKey(e.Path)
		ie.skey = StripPrefix(ie.key)
	}
	return ie
}

// addEntries indexes the entries of every file (repositories in name
// order) and counts the files whose extraction failed.
func (x *Index) addEntries(files map[string][]FileRecord) {
	names := make([]string, 0, len(files))
	for r := range files {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		for _, f := range files[r] {
			if f.Error != "" {
				x.FileErrors[r]++
			}
			for _, e := range f.Entries {
				if e.Side == Provides && mockPath(e.File) {
					continue // a mock or a fixture imitates a provider
				}
				x.Entries = append(x.Entries, indexEntry(e, r))
			}
		}
	}
}

// dropClientSpecs drops the spec endpoints of client repositories: a
// repository with calls in code and no route in code (a frontend) that
// keeps an OpenAPI/Swagger file keeps another service's contract, not its
// own API. A repository with only a spec (contract-first) or with code
// routes keeps its spec endpoints.
func (x *Index) dropClientSpecs() {
	route, call := map[string]bool{}, map[string]bool{}
	for _, e := range x.Entries {
		switch {
		case e.Side == Provides && e.Source != SourceOpenAPI:
			route[lower(e.Repo)] = true
		case e.Side == Consumes && e.Source != SourceOpenAPI:
			call[lower(e.Repo)] = true
		}
	}
	x.clients = map[string]bool{}
	for r := range call {
		if !route[r] {
			x.clients[r] = true
		}
	}
	kept := x.Entries[:0]
	for _, e := range x.Entries {
		if e.Side == Provides && e.Source == SourceOpenAPI && x.clients[lower(e.Repo)] {
			continue
		}
		kept = append(kept, e)
	}
	x.Entries = kept
}

// buildProviderTables fills the provider lookup tables: by path key (as
// written and without an API prefix) and by event.
func (x *Index) buildProviderTables() {
	x.exact, x.stripped, x.events = map[string][]provider{}, map[string][]provider{}, map[string][]string{}
	for _, e := range x.Entries {
		if e.Side != Provides {
			continue
		}
		if e.key != "" {
			p := provider{e.Repo, e.Method, e.Source == SourceOpenAPI}
			x.exact[family(e.Protocol)+"|"+e.key] = append(x.exact[family(e.Protocol)+"|"+e.key], p)
			x.stripped[family(e.Protocol)+"|"+e.skey] = append(x.stripped[family(e.Protocol)+"|"+e.skey], p)
		}
		if e.Event != "" {
			k := string(e.Protocol) + "|" + EventKey(e.Event)
			x.events[k] = append(x.events[k], e.Repo)
		}
	}
}

// buildPairs records the repositories connected by path, per protocol
// family (after pass 1).
func (x *Index) buildPairs() {
	x.pairs, x.connected = map[string]map[string]bool{}, map[string]map[string]bool{}
	link := func(a, b string) {
		if x.connected[a] == nil {
			x.connected[a] = map[string]bool{}
		}
		x.connected[a][b] = true
	}
	for _, e := range x.Entries {
		if e.Side == Consumes && e.Status == Resolved && e.key != "" {
			link(lower(e.Repo), lower(e.Target))
			link(lower(e.Target), lower(e.Repo))
			k := family(e.Protocol) + "|" + lower(e.Repo)
			if x.pairs[k] == nil {
				x.pairs[k] = map[string]bool{}
			}
			x.pairs[k][lower(e.Target)] = true
		}
	}
}

// buildEdges counts the resolved consumers per repository pair.
func (x *Index) buildEdges() {
	edges := map[string]*Edge{}
	for _, e := range x.Entries {
		if e.Side != Consumes || e.Status != Resolved && e.Status != Topology {
			continue
		}
		k := lower(e.Repo) + "->" + lower(e.Target)
		ed := edges[k]
		if ed == nil {
			ed = &Edge{From: e.Repo, To: e.Target, Counts: map[Protocol]int{}}
			edges[k] = ed
		}
		if e.Status == Topology {
			if ed.RuleCounts == nil {
				ed.RuleCounts = map[Protocol]int{}
			}
			ed.RuleCounts[e.Protocol]++
			ed.RuleTotal++
			continue
		}
		ed.Counts[e.Protocol]++
		ed.Total++
	}
	for _, ed := range edges {
		x.Edges = append(x.Edges, *ed)
	}
	sort.Slice(x.Edges, func(i, j int) bool {
		if x.Edges[i].From != x.Edges[j].From {
			return x.Edges[i].From < x.Edges[j].From
		}
		return x.Edges[i].To < x.Edges[j].To
	})
}

// reposOf returns the provider repositories of ps compatible with method
// for a consumer in self (lower case). An OpenAPI/Swagger description is
// weaker than a route in code: the consumer's own repository is not a
// provider of its calls when it only keeps such a description (a frontend
// with the backend's swagger.json), and of several providers the only one
// that serves the route in code wins.
func reposOf(ps []provider, method, self string) []string {
	set, code := map[string]string{}, map[string]string{}
	for _, p := range ps {
		if methodsMatch(method, p.method) {
			set[lower(p.repo)] = p.repo
			if !p.spec {
				code[lower(p.repo)] = p.repo
			}
		}
	}
	if _, ok := code[self]; !ok || len(set) > 1 {
		// The caller's own repository only as a last resort: a service rarely
		// calls its own endpoint over the network.
		delete(set, self)
		delete(code, self)
	}
	if len(set) > 1 && len(code) == 1 {
		return sortedValues(code)
	}
	return sortedValues(set)
}

// matchByHint resolves a consumer by a hint (rule 1 of Build); false when
// no hint applies.
func (x *Index) matchByHint(e *IndexedEntry) bool {
	repo := matchHint(x.hintKeys, x.Hints, e.Base, e.Raw)
	if repo == "" {
		return false
	}
	e.Status, e.Target, e.Via = Resolved, repo, ViaHint
	if lower(repo) == lower(e.Repo) {
		e.Status = Internal
	}
	return true
}

// markedExternal applies the model's third-party mark (rule 5 of Build);
// a mark on a host of the organization's domain is dropped.
func (x *Index) markedExternal(e *IndexedEntry) bool {
	if e.ExternalHost == "" {
		return false
	}
	if underDomain(e.ExternalHost, x.ownDomain) {
		e.ExternalHost, e.ExternalService = "", ""
		return false
	}
	e.Status = External
	return true
}

// buildOwnHosts records the URL hosts of the consumers matched to a
// repository: such a host belongs to the team (after both passes).
func (x *Index) buildOwnHosts() {
	x.ownHosts = map[string]bool{}
	for _, e := range x.Entries {
		if e.urlHost != "" && (e.Status == Resolved || e.Status == Internal) {
			x.ownHosts[e.urlHost] = true
		}
	}
}

// external turns an unresolved consumer with a literal public URL into an
// external one unless a matched call uses the same host (rule 5 of Build).
func (x *Index) external(e *IndexedEntry) {
	if e.Status == Unresolved && e.urlHost != "" && !x.ownHosts[e.urlHost] && !underDomain(e.urlHost, x.ownDomain) {
		e.Status, e.Reason, e.Candidates, e.ExternalHost = External, "", nil, e.urlHost
	}
}

// matchPath resolves a consumer with a path (rules 1-3 and 5 of Build).
func (x *Index) matchPath(e *IndexedEntry) {
	if x.externalByConfig(e, x.matchPathKey) || x.matchByHint(e) || x.markedExternal(e) {
		return
	}
	x.matchPathKey(e)
}

// matchPathKey matches a consumer by its path key (rules 2-3 of Build).
func (x *Index) matchPathKey(e *IndexedEntry) {
	self := lower(e.Repo)
	if genericKey(e.key) {
		e.Status, e.Reason = Unresolved, "слишком общий путь"
		return
	}
	fam := family(e.Protocol)
	cands, via := reposOf(x.exact[fam+"|"+e.key], e.Method, self), ViaPath
	if len(cands) == 0 && !genericKey(e.skey) {
		cands, via = reposOf(x.stripped[fam+"|"+e.skey], e.Method, self), ViaPrefix
	}
	switch {
	case len(cands) == 1 && lower(cands[0]) == self:
		e.Status, e.Target, e.Via = Internal, cands[0], via
	case len(cands) == 1:
		e.Status, e.Target, e.Via = Resolved, cands[0], via
	case len(cands) == 0:
		e.Status, e.Reason = Unresolved, ReasonNoProvider
	default:
		e.Status, e.Reason, e.Candidates = Unresolved, "несколько провайдеров", cands
	}
}

// matchEvent resolves an event-only consumer (rules 1, 4 and 5 of Build).
func (x *Index) matchEvent(e *IndexedEntry) {
	if x.externalByConfig(e, x.matchEventName) || x.matchByHint(e) || x.markedExternal(e) {
		return
	}
	x.matchEventName(e)
}

// matchEventName matches an event-only consumer by name (rule 4 of Build).
func (x *Index) matchEventName(e *IndexedEntry) {
	self := lower(e.Repo)
	set := map[string]string{}
	for _, r := range x.events[string(e.Protocol)+"|"+EventKey(e.Event)] {
		set[lower(r)] = r
	}
	all := sortedValues(set)
	var paired []string
	for _, r := range all {
		if x.pairs[family(e.Protocol)+"|"+self][lower(r)] {
			paired = append(paired, r)
		}
	}
	switch {
	case len(paired) == 1:
		e.Status, e.Target, e.Via = Resolved, paired[0], ViaPair
	case len(paired) > 1:
		e.Status, e.Reason, e.Candidates = Unresolved, "несколько провайдеров события", paired
	case len(all) == 1 && lower(all[0]) == self:
		e.Status, e.Target, e.Via = Internal, all[0], ViaEvent
	case len(all) == 1 && specificEvent(e.Event):
		e.Status, e.Target, e.Via = Resolved, all[0], ViaEvent
	case len(all) == 0 && x.matchEventSuffix(e):
	case len(all) == 0:
		e.Status, e.Reason = Unresolved, ReasonNoEventProvider
	case len(all) == 1:
		e.Status, e.Reason, e.Candidates = Unresolved, "общее имя события, нет связи по пути", all
	default:
		e.Status, e.Reason, e.Candidates = Unresolved, "несколько провайдеров события", all
	}
}

// TopologyFromConfig converts review.sysmap.topology (validated) into
// rules; nil when there are none.
func TopologyFromConfig(rules []config.SysmapTopologyRule) []TopologyRule {
	var out []TopologyRule
	for _, r := range rules {
		n := r.Normalized()
		tr := TopologyRule{From: n.From, To: n.To}
		for _, v := range n.Via {
			tr.Via = append(tr.Via, Protocol(v))
		}
		out = append(out, tr)
	}
	return out
}

// byTopology links an unresolved consumer by the topology rules (rule 6
// of Build): the rules of its repository and protocol must name exactly
// one target repository (other than its own). The target among the call's
// candidates lands on that endpoint (ViaTopology); else the call links
// the repositories only (ViaTopologyEdge). Matches found in code are never
// changed.
func (x *Index) byTopology(e *IndexedEntry) {
	if e.Side != Consumes || e.Status != Unresolved || len(x.Topology) == 0 || thirdPartyPath(e.File) {
		return
	}
	self := lower(e.Repo)
	target, rule := "", -1
	for i, r := range x.Topology {
		if lower(r.From) != self || lower(r.To) == self || !slices.Contains(r.Via, e.Protocol) {
			continue
		}
		if target != "" && lower(target) != lower(r.To) {
			return // two targets: the rules do not decide
		}
		if target == "" {
			target, rule = r.To, i
		}
	}
	if target == "" {
		return
	}
	via := ViaTopologyEdge
	if containsFold(e.Candidates, lower(target)) {
		via = ViaTopology
	}
	e.Status, e.Target, e.Via, e.Reason, e.Candidates = Topology, target, via, "", nil
	x.TopologyHits[rule]++
}

// matchEventSuffix resolves a webhook sent without a path (a dynamic
// callback URL) to a receiver whose event name ends with the sent one or
// the other way round ("usage" -> "llm.usage"): only among the
// repositories already connected to the sender by a path call, and only
// when exactly one of them has exactly one such event (a receiver with a
// path). Anything else stays unresolved: no guessing.
func (x *Index) matchEventSuffix(e *IndexedEntry) bool {
	if e.Protocol != Webhook {
		return false
	}
	self := lower(e.Repo)
	found := map[string]map[string]bool{} // repo -> event keys
	names := map[string]string{}
	for _, p := range x.Entries {
		r := lower(p.Repo)
		if p.Side != Provides || p.Protocol != Webhook || p.key == "" || r == self || !x.connected[self][r] || !eventSuffix(e.Event, p.Event) {
			continue
		}
		if found[r] == nil {
			found[r] = map[string]bool{}
		}
		found[r][EventKey(p.Event)], names[r] = true, p.Repo
	}
	if len(found) != 1 {
		return false
	}
	for r, evs := range found {
		if len(evs) != 1 {
			return false
		}
		e.Status, e.Target, e.Via = Resolved, names[r], ViaEventSuffix
	}
	return true
}

// specificEvent: an event name with a separator ("order.paid",
// "orders:updated") is specific enough to match across the whole map; a
// bare word ("message", "update") is not.
func specificEvent(e string) bool {
	return len(e) >= 5 && strings.ContainsAny(e, ".:/_-")
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return lower(out[i]) < lower(out[j]) })
	return out
}

func sortedHintKeys(h map[string]string) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		if strings.TrimSpace(k) != "" {
			keys = append(keys, k)
		}
	}
	// Longest first: "ORDERS_URL_V2" before "ORDERS_URL".
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

// matchHint returns the repository of the first hint key found as a whole
// token in base or raw ("" when none).
func matchHint(keys []string, hints map[string]string, base, raw string) string {
	for _, k := range keys {
		if containsToken(base, k) || containsToken(raw, k) {
			return strings.Trim(strings.TrimSpace(hints[k]), "/")
		}
	}
	return ""
}

// containsToken reports whether tok occurs in s (case-insensitive) not
// glued to identifier characters on either side.
func containsToken(s, tok string) bool {
	if s == "" || tok == "" {
		return false
	}
	ls, lt := strings.ToLower(s), strings.ToLower(tok)
	for from := 0; ; {
		i := strings.Index(ls[from:], lt)
		if i < 0 {
			return false
		}
		i += from
		j := i + len(lt)
		if (i == 0 || !identChar(ls[i-1]) || !identChar(lt[0])) && (j == len(ls) || !identChar(ls[j]) || !identChar(lt[len(lt)-1])) {
			return true
		}
		from = i + 1
	}
}

func identChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z'
}

// Repo returns the scan state of a repository (nil when not in the map).
func (x *Index) Repo(path string) *RepoState {
	if x == nil {
		return nil
	}
	for i := range x.Repos {
		if lower(x.Repos[i].Path) == lower(path) {
			return &x.Repos[i]
		}
	}
	return nil
}
