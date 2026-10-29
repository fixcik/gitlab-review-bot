package sysmap

import (
	"context"
	"strings"
	"testing"
	"time"
)

// depsAnswer is a deterministic "model" for merge-request files: a line
// "// CALL <METHOD> <path> [base]" is a call, "// ROUTE <METHOD> <path>" a
// route, "// LISTEN <event>" an SSE subscription.
func depsAnswer(file, content string) []rawEntry {
	var out []rawEntry
	for i, l := range strings.Split(content, "\n") {
		_, rest, ok := strings.Cut(l, "// ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		switch {
		case len(f) >= 3 && f[0] == "CALL":
			e := rawEntry{File: file, Line: i + 1, Side: "consumes", Protocol: "http", Method: f[1], Path: f[2], Raw: f[2]}
			if len(f) > 3 {
				e.Base = f[3]
			}
			out = append(out, e)
		case len(f) >= 3 && f[0] == "ROUTE":
			out = append(out, rawEntry{File: file, Line: i + 1, Side: "provides", Protocol: "http", Method: f[1], Path: f[2], Raw: f[2]})
		case len(f) >= 2 && f[0] == "LISTEN":
			out = append(out, rawEntry{File: file, Line: i + 1, Side: "consumes", Protocol: "sse", Event: f[1], Raw: f[1]})
		}
	}
	return out
}

type depsEnv struct {
	t   *testing.T
	st  *memStore
	run *extractRunner
	m   *Map
	now time.Time
}

func newDepsEnv(t *testing.T, maxRuns int) *depsEnv {
	t.Helper()
	e := &depsEnv{t: t, st: newMemStore(), now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	e.run = &extractRunner{answer: depsAnswer}
	cons := func(file string, line int, method, p, base string) Entry {
		return Entry{Side: Consumes, Protocol: HTTP, Method: method, Path: p, Base: base, File: file, Line: line, Source: SourceCode}
	}
	route := func(file string, line int, method, p string) Entry {
		return Entry{Side: Provides, Protocol: HTTP, Method: method, Path: p, File: file, Line: line, Source: SourceCode}
	}
	ctx := context.Background()
	for _, r := range []RepoState{
		{Path: "g/web", SHA: "w1", Branch: "main", Status: StatusOK},
		{Path: "g/orders", SHA: "o1", Branch: "main", Status: StatusOK},
		{Path: "g/users", SHA: "u1", Branch: "main", Status: StatusOK},
		{Path: "g/partial", SHA: "p1", Branch: "main", Status: StatusPartial},
	} {
		_ = e.st.SaveSysmapRepo(ctx, r)
	}
	_ = e.st.SaveSysmapFiles(ctx, "g/orders", []FileRecord{{Path: "routes.py", Hash: "h-orders", Version: CodeVersion, Entries: []Entry{
		route("routes.py", 10, "GET", "/orders/{id}"), route("routes.py", 30, "POST", "/orders"),
	}}})
	_ = e.st.SaveSysmapFiles(ctx, "g/users", []FileRecord{{Path: "app.php", Hash: "h-users", Version: CodeVersion, Entries: []Entry{
		route("app.php", 5, "GET", "/users/{id}"), route("app.php", 9, "GET", "/users/{id}/roles"),
	}}})
	_ = e.st.SaveSysmapFiles(ctx, "g/web", []FileRecord{{Path: "src/api.ts", Hash: "h-web", Version: CodeVersion, Entries: []Entry{
		cons("src/api.ts", 3, "GET", "/orders/{id}", "ORDERS_URL"),
		cons("src/api.ts", 4, "GET", "/users/{id}", "API"),
		cons("src/api.ts", 5, "POST", "/orders", "API"),
		cons("src/api.ts", 6, "GET", "/local", ""),
		route("src/server.ts", 1, "GET", "/local"),
	}}})
	e.m = New(Options{Store: e.st, Runner: e.run, Model: "sonnet", ScratchDir: t.TempDir(), MaxRunsPerHour: maxRuns, DepsMaxRunsPerHour: maxRuns,
		Now: func() time.Time { return e.now }})
	if err := e.m.Load(ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *depsEnv) deps(req DepsRequest) *DepsResult {
	e.t.Helper()
	if req.Repo == "" {
		req.Repo = "g/web"
	}
	res, err := e.m.Dependencies(context.Background(), req)
	if err != nil {
		e.t.Fatal(err)
	}
	if res == nil {
		e.t.Fatal("nil result")
	}
	return res
}

func byRole(res *DepsResult, role DepRole) []DepContract {
	var out []DepContract
	for _, c := range res.Contracts {
		if c.Role == role {
			out = append(out, c)
		}
	}
	return out
}

// webFile is a changed file whose lines 1..n are all added.
func webFile(lines ...string) (DepsFile, map[string][]LineRange) {
	content := "import x from 'y'\n" + strings.Join(lines, "\n") + "\n"
	return DepsFile{Path: "src/new.ts", Content: []byte(content)}, map[string][]LineRange{"src/new.ts": {{From: 2, To: len(lines) + 1}}}
}

func TestDepsConsumerSide(t *testing.T) {
	e := newDepsEnv(t, 20)
	f, added := webFile(
		"fetch(a) // CALL GET /orders/{orderId}/history ORDERS_URL", // path nowhere -> base hint -> g/orders, missing
		"fetch(b) // CALL GET /users/{uid}/roles",                   // resolved by path -> in main
		"fetch(c) // CALL GET /mystery API",                         // base API -> g/users and g/orders: ambiguous
		"fetch(d) // CALL GET /local",                               // internal
		"fetch(e) // CALL GET /orders/{id} ORDERS_URL",              // already called on main
		"fetch(f) // CALL GET /nowhere",                             // no base: unknown provider
		"fetch(g) // ROUTE POST /web-hook",                          // provides-new
	)
	res := e.deps(DepsRequest{Files: []DepsFile{f}, Added: added})
	if res.Runs != 1 || res.Extracted != 1 || res.Cached != 0 {
		t.Errorf("runs %d extracted %d cached %d", res.Runs, res.Extracted, res.Cached)
	}
	cons := byRole(res, RoleConsumesNew)
	if len(cons) != 2 {
		t.Fatalf("consumes-new = %+v", cons)
	}
	if c := cons[0]; c.Provider != "g/orders" || c.Via != ViaBase || c.InProviderMain || !c.ProviderComplete || c.Name() != "GET /orders/{orderId}/history" || c.Line != 2 {
		t.Errorf("base-hint contract = %+v", c)
	}
	if c := cons[1]; c.Provider != "g/users" || c.Via != ViaPath || !c.InProviderMain {
		t.Errorf("path contract = %+v", c)
	}
	if p := byRole(res, RoleProvidesNew); len(p) != 1 || p[0].Name() != "POST /web-hook" {
		t.Errorf("provides-new = %+v", p)
	}
	if len(res.Consumers) != 0 {
		t.Errorf("nobody calls g/web: %v", res.Consumers)
	}
	if len(e.st.runs) != 1 || e.st.runs[0].Repo != "g/web" {
		t.Errorf("runs must be recorded for the budget: %+v", e.st.runs)
	}
	if !strings.Contains(e.run.reqs[0].Prompt, "merge request head") || e.run.reqs[0].Model != "sonnet" || !e.run.reqs[0].NoTools {
		t.Errorf("request: %+v", e.run.reqs[0])
	}

	// Same content again (another branch or repository): from the cache.
	res = e.deps(DepsRequest{Repo: "g/web", Files: []DepsFile{f}, Added: added})
	if res.Runs != 0 || res.Cached != 1 || len(byRole(res, RoleConsumesNew)) != 2 {
		t.Errorf("cached: runs %d cached %d %+v", res.Runs, res.Cached, res.Contracts)
	}
}

func TestDepsUnchangedLinesIgnored(t *testing.T) {
	e := newDepsEnv(t, 20)
	f, _ := webFile("fetch(a) // CALL GET /orders/{id}/history ORDERS_URL")
	res := e.deps(DepsRequest{Files: []DepsFile{f}, Added: map[string][]LineRange{"src/new.ts": {{From: 40, To: 41}}}})
	if len(res.Contracts) != 0 {
		t.Errorf("entries off the changed lines: %+v", res.Contracts)
	}
}

func TestDepsProviderIncomplete(t *testing.T) {
	e := newDepsEnv(t, 20)
	e.m.o.Hints = map[string]string{"PARTIAL_URL": "g/partial"}
	_ = e.m.Load(context.Background())
	f, added := webFile("fetch(a) // CALL GET /p/thing PARTIAL_URL")
	res := e.deps(DepsRequest{Files: []DepsFile{f}, Added: added})
	c := byRole(res, RoleConsumesNew)
	if len(c) != 1 || c[0].Via != ViaHint || c[0].InProviderMain || c[0].ProviderComplete {
		t.Errorf("contracts = %+v", res.Contracts)
	}
}

func TestDepsDropped(t *testing.T) {
	e := newDepsEnv(t, 20)
	// src/api.ts at the head no longer calls GET /users/{id} (line 4).
	content := "a\nb\nfetch(x) // CALL GET /orders/{id} ORDERS_URL\n\nfetch(y) // CALL POST /orders API\n"
	res := e.deps(DepsRequest{
		Files:   []DepsFile{{Path: "src/api.ts", Content: []byte(content)}},
		Added:   map[string][]LineRange{"src/api.ts": {{From: 4, To: 4}}},
		Removed: map[string][]LineRange{"src/api.ts": {{From: 4, To: 4}}},
	})
	d := byRole(res, RoleConsumesDropped)
	if len(d) != 1 || d[0].Provider != "g/users" || d[0].Name() != "GET /users/{id}" {
		t.Errorf("dropped = %+v (all %+v)", d, res.Contracts)
	}
	// A deleted file drops its calls without extraction.
	res = e.deps(DepsRequest{Deleted: []string{"src/api.ts"}, Removed: map[string][]LineRange{"src/api.ts": {{From: 1, To: 9}}}})
	if d := byRole(res, RoleConsumesDropped); len(d) != 3 || res.Runs != 0 {
		t.Errorf("deleted file: %+v", res.Contracts)
	}
}

func TestDepsBudgetExhausted(t *testing.T) {
	// Runs of the map (main branches) do not take phase B's budget.
	sep := newDepsEnv(t, 1)
	_ = sep.st.RecordSysmapRun(context.Background(), RunRecord{At: sep.now.Add(-10 * time.Minute), Repo: "g/x", OK: true, Kind: RunKindScan})
	f, added := webFile("fetch(a) // CALL GET /orders/{id}/history ORDERS_URL")
	if res := sep.deps(DepsRequest{Files: []DepsFile{f}, Added: added}); res.Runs != 1 {
		t.Errorf("map run took the phase B budget: %+v", res)
	}
	sep.st.mu.Lock()
	last := sep.st.runs[len(sep.st.runs)-1]
	sep.st.mu.Unlock()
	if last.Kind != RunKindDeps {
		t.Errorf("phase B run recorded as %q", last.Kind)
	}
	e := newDepsEnv(t, 1)
	_ = e.st.RecordSysmapRun(context.Background(), RunRecord{At: e.now.Add(-10 * time.Minute), Repo: "g/x", OK: true, Kind: RunKindDeps})
	res := e.deps(DepsRequest{Files: []DepsFile{f}, Added: added})
	if res.Runs != 0 || len(e.run.reqs) != 0 || res.Skipped != 1 || res.SkipReason != "budget exhausted" || len(res.Contracts) != 0 {
		t.Errorf("budget: %+v", res)
	}
	off := newDepsEnv(t, 0)
	res = off.deps(DepsRequest{Files: []DepsFile{f}, Added: added})
	if res.Runs != 0 || res.SkipReason != "code extraction off" {
		t.Errorf("off: %+v", res)
	}
}

func TestDepsCapsAndCandidates(t *testing.T) {
	e := newDepsEnv(t, 20)
	var files []DepsFile
	added := map[string][]LineRange{}
	for _, p := range []string{"a.ts", "b.ts", "c.ts", "node_modules/x.js", "src/a.test.ts", "README.md"} {
		files = append(files, DepsFile{Path: p, Content: []byte("fetch(x) // CALL GET /users/{id}\n")})
		added[p] = []LineRange{{From: 1, To: 1}}
	}
	res := e.deps(DepsRequest{Files: files, Added: added, MaxFiles: 2})
	if got := e.run.sentFiles(); strings.Join(got, ",") != "a.ts,b.ts" || res.Skipped != 1 {
		t.Errorf("sent %v skipped %d", got, res.Skipped)
	}
	if e.m.Index() == nil {
		t.Fatal("no index")
	}
	if r, _ := e.m.Dependencies(context.Background(), DepsRequest{Repo: "g/unknown"}); r != nil {
		t.Error("a repo outside the map gives nil")
	}
	for p, want := range map[string]bool{"src/a.ts": true, "api/openapi.yaml": true, "vendor/x.php": false, ".github/x.js": false,
		"tests/a.py": false, "package.json": false, "a_test.go": false} {
		if DepsCandidate(p) != want {
			t.Errorf("DepsCandidate(%q) != %v", p, want)
		}
	}
}

func TestDepsSpecAndConsumers(t *testing.T) {
	e := newDepsEnv(t, 20)
	spec := "openapi: 3.0.0\ninfo: {title: o, version: \"1\"}\npaths:\n  /orders/{id}:\n    get: {}\n  /orders/{id}/history:\n    get: {}\n"
	res := e.deps(DepsRequest{Repo: "g/orders", Files: []DepsFile{{Path: "openapi.yaml", Content: []byte(spec)}},
		Added: map[string][]LineRange{"openapi.yaml": {{From: 6, To: 7}}}})
	p := byRole(res, RoleProvidesNew)
	if len(p) != 1 || p[0].Name() != "GET /orders/{id}/history" || res.Runs != 0 {
		t.Errorf("spec provides-new: %+v", res.Contracts)
	}
	if strings.Join(res.Consumers, ",") != "g/web" {
		t.Errorf("consumers = %v", res.Consumers)
	}
}
