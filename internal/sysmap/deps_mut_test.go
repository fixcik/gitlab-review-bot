package sysmap

import (
	"context"
	"strings"
	"testing"
	"time"
)

// addFiles stores more file records in a deps env and rebuilds the index
// with hints.
func (e *depsEnv) addFiles(hints map[string]string, repo string, recs ...FileRecord) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.st.SaveSysmapFiles(ctx, repo, recs); err != nil {
		e.t.Fatal(err)
	}
	e.m.o.Hints = hints
	if err := e.m.Load(ctx); err != nil {
		e.t.Fatal(err)
	}
}

func contractNames(cs []DepContract) string {
	var out []string
	for _, c := range cs {
		out = append(out, string(c.Role)+" "+c.Name()+" "+c.Provider)
	}
	return strings.Join(out, "; ")
}

func TestDepsSkippedFilesDoNotStopTheRest(t *testing.T) {
	e := newDepsEnv(t, 20)
	spec := "openapi: 3.0.0\ninfo: {title: w, version: \"1\"}\npaths:\n  /brand-new:\n    get: {}\n"
	files := []DepsFile{
		{Path: "README.md", Content: []byte("fetch(x)")},
		{Path: "docs/settings.yaml", Content: []byte("name: x\n")},
		{Path: "docs/broken.yaml", Content: []byte("openapi: 3.0.0\npaths:\n  /x: [unclosed\n")},
		{Path: "openapi.yaml", Content: []byte(spec)},
		{Path: "bin.ts", Content: []byte("fetch(x) // CALL GET /users/{id}/roles\x00\n")},
		{Path: "plain.ts", Content: []byte("export const a = 1\n")},
		{Path: "src/new.ts", Content: []byte("fetch(x) // CALL GET /users/{id}/roles\n")},
	}
	added := map[string][]LineRange{}
	for _, f := range files {
		added[f.Path] = []LineRange{{From: 1, To: 9}}
	}
	res := e.deps(DepsRequest{Files: files, Added: added})
	if got := contractNames(res.Contracts); got != "provides-new GET /brand-new ; consumes-new GET /users/{id}/roles g/users" {
		t.Errorf("contracts = %s", got)
	}
	if got := e.run.sentFiles(); strings.Join(got, ",") != "src/new.ts" {
		t.Errorf("sent = %v", got)
	}
}

func TestDepsCachedAndExtractedMix(t *testing.T) {
	e := newDepsEnv(t, 1)
	a := DepsFile{Path: "src/a.ts", Content: []byte("fetch(x) // CALL GET /users/{id}/roles\n")}
	b := DepsFile{Path: "src/b.ts", Content: []byte("fetch(x) // CALL GET /orders/{id}/lines ORDERS_URL\n")}
	added := map[string][]LineRange{"src/a.ts": {{From: 1, To: 1}}, "src/b.ts": {{From: 1, To: 1}}}
	if res := e.deps(DepsRequest{Files: []DepsFile{a}, Added: added}); res.Runs != 1 {
		t.Fatalf("first: %+v", res)
	}
	// The budget is spent; a's result comes from the cache, nothing is
	// skipped.
	res := e.deps(DepsRequest{Files: []DepsFile{a}, Added: added})
	if res.Cached != 1 || res.Skipped != 0 || res.SkipReason != "" || len(res.Contracts) != 1 {
		t.Errorf("all cached with no budget left: %+v", res)
	}
	e.now = e.now.Add(2 * time.Hour)
	res = e.deps(DepsRequest{Files: []DepsFile{a, b}, Added: added})
	if res.Cached != 1 || res.Extracted != 1 || len(res.Contracts) != 2 {
		t.Errorf("cached a, extracted b: %+v %s", res, contractNames(res.Contracts))
	}
}

func TestDepsBatchesBeyondRunCapSkipped(t *testing.T) {
	e := newDepsEnv(t, 20)
	e.m.o.MaxBatchFiles = 1
	var files []DepsFile
	added := map[string][]LineRange{}
	for _, p := range []string{"a.ts", "b.ts", "c.ts"} {
		files = append(files, DepsFile{Path: p, Content: []byte("fetch(x) // CALL GET /users/{id}/roles " + p + "\n")})
		added[p] = []LineRange{{From: 1, To: 1}}
	}
	res := e.deps(DepsRequest{Files: files, Added: added, MaxRuns: 1})
	if res.Runs != 1 || res.Skipped != 2 || res.SkipReason != "budget exhausted" {
		t.Errorf("run cap: %+v", res)
	}
}

func TestDepsSameContractOnce(t *testing.T) {
	e := newDepsEnv(t, 20)
	files := []DepsFile{
		{Path: "a.ts", Content: []byte("fetch(x) // CALL GET /users/{id}/roles\n")},
		{Path: "b.ts", Content: []byte("fetch(x) // CALL GET /users/{uid}/roles\n")},
	}
	added := map[string][]LineRange{"a.ts": {{From: 1, To: 1}}, "b.ts": {{From: 1, To: 1}}}
	if res := e.deps(DepsRequest{Files: files, Added: added}); len(res.Contracts) != 1 {
		t.Errorf("duplicate contracts: %s", contractNames(res.Contracts))
	}
}

func TestDepsResolutionRules(t *testing.T) {
	e := newDepsEnv(t, 20)
	ev := Entry{Side: Provides, Protocol: SSE, Event: "order.updated", File: "events.py", Line: 1, Source: SourceCode}
	e.addFiles(map[string]string{"EXT_URL": "g/external"}, "g/orders",
		FileRecord{Path: "events.py", Hash: "h-ev", Version: CodeVersion, Entries: []Entry{ev,
			{Side: Provides, Protocol: HTTP, Method: "GET", Path: "/shared/{id}", File: "events.py", Line: 2}}},
		FileRecord{Path: "client.py", Hash: "h-cl", Version: CodeVersion, Entries: []Entry{
			{Side: Consumes, Protocol: HTTP, Method: "GET", Path: "/users/{id}", File: "client.py", Line: 3}}})
	e.addFiles(e.m.o.Hints, "g/users", FileRecord{Path: "shared.php", Hash: "h-sh", Version: CodeVersion, Entries: []Entry{
		{Side: Provides, Protocol: HTTP, Method: "GET", Path: "/shared/{id}", File: "shared.php", Line: 1}}})

	f, added := webFile(
		"es.on(x) // LISTEN order.updated",                    // event provided by exactly one repository
		"fetch(a) // CALL GET /ext/thing EXT_URL",             // hint to a repository outside the map
		"fetch(b) // CALL GET /shared/{x} ORDERS_URL",         // ambiguous path: the base must not decide
		"fetch(c) // CALL GET /api/v1/orders/{id} ORDERS_URL", // main already calls it without the prefix
	)
	res := e.deps(DepsRequest{Files: []DepsFile{f}, Added: added})
	cons := byRole(res, RoleConsumesNew)
	if len(cons) != 1 || cons[0].Name() != "order.updated" || cons[0].Protocol != SSE || cons[0].Provider != "g/orders" || cons[0].Via != ViaEvent {
		t.Errorf("consumes-new = %s", contractNames(cons))
	}

	// g/orders' only base-less call goes to g/users: a base-less new call
	// to an unknown path is still not attributed.
	res = e.deps(DepsRequest{Repo: "g/orders", Files: []DepsFile{{Path: "n.py", Content: []byte("requests.get(x) // CALL GET /nowhere/at/all\n")}},
		Added: map[string][]LineRange{"n.py": {{From: 1, To: 1}}}})
	if len(res.Contracts) != 0 {
		t.Errorf("base-less call attributed: %s", contractNames(res.Contracts))
	}
}

func TestDepsDroppedOnlyOwnResolvedCalls(t *testing.T) {
	e := newDepsEnv(t, 20)
	e.addFiles(nil, "g/orders", FileRecord{Path: "items.py", Hash: "h-it", Version: CodeVersion, Entries: []Entry{
		{Side: Provides, Protocol: HTTP, Method: "GET", Path: "/orders/{id}/items", File: "items.py", Line: 1}}})
	// Another repository has a file with the same path.
	e.addFiles(map[string]string{"EXT_URL": "g/external"}, "g/users", FileRecord{Path: "src/api.ts", Hash: "h-u2", Version: CodeVersion, Entries: []Entry{
		{Side: Consumes, Protocol: HTTP, Method: "GET", Path: "/orders/{id}/items", File: "src/api.ts", Line: 2}}})
	// g/web's first call goes to a repository outside the map.
	e.addFiles(e.m.o.Hints, "g/web", FileRecord{Path: "src/api.ts", Hash: "h-web", Version: CodeVersion, Entries: []Entry{
		{Side: Consumes, Protocol: HTTP, Method: "GET", Path: "/ext", Base: "EXT_URL", File: "src/api.ts", Line: 1},
		{Side: Consumes, Protocol: HTTP, Method: "GET", Path: "/orders/{id}", Base: "ORDERS_URL", File: "src/api.ts", Line: 3},
	}})
	res := e.deps(DepsRequest{Deleted: []string{"src/api.ts"}, Removed: map[string][]LineRange{"src/api.ts": {{From: 1, To: 9}}}})
	if got := contractNames(res.Contracts); got != "consumes-dropped GET /orders/{id} g/orders" {
		t.Errorf("dropped = %s", got)
	}
}
