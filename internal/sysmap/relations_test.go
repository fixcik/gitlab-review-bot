package sysmap

import (
	"reflect"
	"testing"
	"time"
)

// relIndex builds an index of one file per repository.
func relIndex(t *testing.T, hints map[string]string, byRepo map[string][]Entry) *Index {
	t.Helper()
	files := map[string][]FileRecord{}
	var repos []RepoState
	for r, es := range byRepo {
		files[r] = []FileRecord{{Path: "f", Entries: es}}
		repos = append(repos, RepoState{Path: r})
	}
	return Build(repos, files, hints, time.Unix(0, 0))
}

// entryAt returns the index of the entry of repo at line.
func entryAt(t *testing.T, x *Index, repo string, line int) int {
	t.Helper()
	for i, e := range x.Entries {
		if e.Repo == repo && e.Line == line {
			return i
		}
	}
	t.Fatalf("no entry %s:%d", repo, line)
	return -1
}

func TestRelations(t *testing.T) {
	prov := func(line int, p Protocol, method, path, event string, src string) Entry {
		return Entry{Side: Provides, Protocol: p, Method: method, Path: path, Event: event, Line: line, Source: src}
	}
	call := func(line int, p Protocol, method, path, event, base string) Entry {
		return Entry{Side: Consumes, Protocol: p, Method: method, Path: path, Event: event, Base: base, Line: line}
	}
	cases := []struct {
		name  string
		hints map[string]string
		repos map[string][]Entry
		// call: the consumer (repo, line) checked
		repo string
		line int
		// lands: provider (repo, line) pairs; noEndpoint; status of the call
		lands      [][2]any
		noEndpoint bool
		status     string
	}{
		{name: "path match", repos: map[string][]Entry{
			"g/p": {prov(1, HTTP, "GET", "/orders/{id}", "", SourceCode)},
			"g/c": {call(10, HTTP, "GET", "/orders/{order_id}", "", "")},
		}, repo: "g/c", line: 10, lands: [][2]any{{"g/p", 1}}, status: Resolved},
		{name: "prefix match", repos: map[string][]Entry{
			"g/p": {prov(1, HTTP, "POST", "/api/v1/orders", "", SourceCode)},
			"g/c": {call(10, HTTP, "POST", "/orders", "", "")},
		}, repo: "g/c", line: 10, lands: [][2]any{{"g/p", 1}}, status: Resolved},
		{name: "empty method matches any", repos: map[string][]Entry{
			"g/p": {prov(1, HTTP, "", "/orders", "", SourceCode)},
			"g/c": {call(10, HTTP, "DELETE", "/orders", "", "")},
		}, repo: "g/c", line: 10, lands: [][2]any{{"g/p", 1}}, status: Resolved},
		{name: "webhook sender lands on an http endpoint", repos: map[string][]Entry{
			"g/p": {prov(1, HTTP, "POST", "/hooks/billing", "", SourceCode)},
			"g/c": {call(10, Webhook, "POST", "/hooks/billing", "invoice.paid", "")},
		}, repo: "g/c", line: 10, lands: [][2]any{{"g/p", 1}}, status: Resolved},
		{name: "hint with the endpoint", hints: map[string]string{"ORDERS_URL": "g/p"}, repos: map[string][]Entry{
			"g/p": {prov(1, HTTP, "GET", "/orders", "", SourceCode)},
			"g/c": {call(10, HTTP, "GET", "/orders", "", "ORDERS_URL")},
		}, repo: "g/c", line: 10, lands: [][2]any{{"g/p", 1}}, status: Resolved},
		{name: "hint without the endpoint", hints: map[string]string{"ORDERS_URL": "g/p"}, repos: map[string][]Entry{
			"g/p": {prov(1, HTTP, "GET", "/orders", "", SourceCode)},
			"g/c": {call(10, HTTP, "GET", "/orders/export", "", "ORDERS_URL")},
		}, repo: "g/c", line: 10, noEndpoint: true, status: Resolved},
		{name: "hint: method differs", hints: map[string]string{"ORDERS_URL": "g/p"}, repos: map[string][]Entry{
			"g/p": {prov(1, HTTP, "GET", "/orders", "", SourceCode)},
			"g/c": {call(10, HTTP, "POST", "/orders", "", "ORDERS_URL")},
		}, repo: "g/c", line: 10, noEndpoint: true, status: Resolved},
		{name: "hint to a repository outside the map", hints: map[string]string{"PAY_URL": "g/unscanned"}, repos: map[string][]Entry{
			"g/c": {call(10, HTTP, "GET", "/pay", "", "PAY_URL")},
		}, repo: "g/c", line: 10, noEndpoint: true, status: Resolved},
		{name: "event-only consumer", repos: map[string][]Entry{
			"g/p": {prov(1, WS, "", "/ws", "", SourceCode), prov(2, WS, "", "", "order.paid", SourceCode)},
			"g/c": {call(10, WS, "", "", "order.paid", "")},
		}, repo: "g/c", line: 10, lands: [][2]any{{"g/p", 2}}, status: Resolved},
		{name: "path and event reach the endpoint and the event", repos: map[string][]Entry{
			"g/p": {prov(1, WS, "", "/ws", "", SourceCode), prov(2, WS, "", "", "user.created", SourceCode), prov(3, WS, "", "", "other.event", SourceCode)},
			"g/c": {call(10, WS, "", "/ws", "user.created", "")},
		}, repo: "g/c", line: 10, lands: [][2]any{{"g/p", 1}, {"g/p", 2}}, status: Resolved},
		{name: "event of another protocol family does not land", hints: map[string]string{"WS_URL": "g/p"}, repos: map[string][]Entry{
			"g/p": {prov(2, SSE, "", "", "user.created", SourceCode)},
			"g/c": {call(10, WS, "", "/ws", "user.created", "WS_URL")},
		}, repo: "g/c", line: 10, noEndpoint: true, status: Resolved},
		{name: "unresolved: lands on candidates, never no-endpoint", repos: map[string][]Entry{
			"g/a": {prov(1, HTTP, "GET", "/health", "", SourceCode)},
			"g/b": {prov(1, HTTP, "GET", "/health", "", SourceCode)},
			"g/c": {call(10, HTTP, "GET", "/health", "", "")},
		}, repo: "g/c", line: 10, lands: [][2]any{{"g/a", 1}, {"g/b", 1}}, status: Unresolved},
		{name: "unresolved without candidates", repos: map[string][]Entry{
			"g/c": {call(10, HTTP, "GET", "/nowhere", "", "")},
		}, repo: "g/c", line: 10, status: Unresolved},
		{name: "internal call lands on its own endpoint", repos: map[string][]Entry{
			"g/c": {prov(1, HTTP, "GET", "/self", "", SourceCode), call(10, HTTP, "GET", "/self", "", "")},
		}, repo: "g/c", line: 10, lands: [][2]any{{"g/c", 1}}, status: Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := relIndex(t, tc.hints, tc.repos)
			rel := x.Relations()
			i := entryAt(t, x, tc.repo, tc.line)
			if x.Entries[i].Status != tc.status {
				t.Fatalf("status %q, want %q (%+v)", x.Entries[i].Status, tc.status, x.Entries[i])
			}
			var want []int
			for _, l := range tc.lands {
				want = append(want, entryAt(t, x, l[0].(string), l[1].(int)))
			}
			if !reflect.DeepEqual(rel.Lands[i], want) {
				t.Errorf("lands %v, want %v", rel.Lands[i], want)
			}
			if rel.NoEndpoint[i] != tc.noEndpoint {
				t.Errorf("no endpoint %v, want %v", rel.NoEndpoint[i], tc.noEndpoint)
			}
			if rel.EndpointOf[i] != -1 {
				t.Errorf("a consumer has no endpoint group: %d", rel.EndpointOf[i])
			}
		})
	}
}

// Endpoints merge the OpenAPI and the code description of one route; the
// same path under another protocol (ws) or method stays apart. Callers are
// the resolved calls of other repositories, once per endpoint; internal
// and unresolved calls are not callers.
func TestRelationsEndpoints(t *testing.T) {
	x := relIndex(t, nil, map[string][]Entry{
		"g/p": {
			{Side: Provides, Protocol: HTTP, Method: "GET", Path: "/api/v1/users/{id}", Line: 1, Source: SourceOpenAPI},
			{Side: Provides, Protocol: HTTP, Method: "GET", Path: "/api/v1/users/{user_id}", Line: 2, Source: SourceCode},
			{Side: Provides, Protocol: HTTP, Method: "POST", Path: "/api/v1/users/{id}", Line: 3, Source: SourceCode},
			{Side: Provides, Protocol: HTTP, Method: "GET", Path: "/ws", Line: 4, Source: SourceCode},
			{Side: Provides, Protocol: WS, Path: "/ws", Line: 5, Source: SourceCode},
			{Side: Consumes, Protocol: HTTP, Method: "GET", Path: "/api/v1/users/{id}", Line: 6},
		},
		"g/a": {
			{Side: Consumes, Protocol: HTTP, Method: "GET", Path: "/api/v1/users/{x}", Line: 10},
			{Side: Consumes, Protocol: HTTP, Method: "GET", Path: "/users/{x}", Line: 11},
		},
	})
	rel := x.Relations()
	if len(rel.Endpoints) != 4 {
		t.Fatalf("endpoints = %+v", rel.Endpoints)
	}
	get := func(line int) EndpointGroup { return rel.Endpoints[rel.EndpointOf[entryAt(t, x, "g/p", line)]] }
	users := get(1)
	if want := []int{entryAt(t, x, "g/p", 1), entryAt(t, x, "g/p", 2)}; !reflect.DeepEqual(users.Entries, want) || users.Repo != "g/p" {
		t.Errorf("users = %+v, want entries %v", users, want)
	}
	if want := []int{entryAt(t, x, "g/a", 10), entryAt(t, x, "g/a", 11)}; !reflect.DeepEqual(users.Callers, want) {
		t.Errorf("callers = %v, want %v (once per call, no internal call)", users.Callers, want)
	}
	if len(get(3).Callers) != 0 || len(get(3).Entries) != 1 {
		t.Errorf("POST endpoint = %+v", get(3))
	}
	if rel.EndpointOf[entryAt(t, x, "g/p", 4)] == rel.EndpointOf[entryAt(t, x, "g/p", 5)] {
		t.Error("http GET /ws and ws /ws are different endpoints")
	}
}

func TestRelationsNilAndLiteral(t *testing.T) {
	var x *Index
	if r := x.Relations(); r == nil || len(r.Endpoints) != 0 {
		t.Errorf("nil index: %+v", r)
	}
	lit := &Index{Entries: []IndexedEntry{indexEntry(Entry{Side: Provides, Protocol: HTTP, Path: "/a"}, "g/p")}}
	if r := lit.Relations(); len(r.Endpoints) != 1 || r.EndpointOf[0] != 0 {
		t.Errorf("index without Build: %+v", r)
	}
}
