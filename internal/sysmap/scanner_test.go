package sysmap

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

const routesPy = `from app import app

@app.post("/orders")
def create_order():
    return {}
`

const apiTS = "import { ORDERS_URL } from './env'\n\nexport function create(o) {\n  return fetch(`${ORDERS_URL}/orders`, { method: 'POST', body: JSON.stringify(o) })\n}\n"

const ordersSpec = `openapi: 3.0.0
info: {title: o, version: "1"}
paths:
  /orders/{id}:
    get:
      operationId: getOrder
`

// answerByContent is a deterministic "model": it reports what the test
// files contain.
func answerByContent(file, content string) []rawEntry {
	var out []rawEntry
	if l := lineOfText(content, "@app.post"); l > 0 {
		out = append(out, rawEntry{File: file, Line: l, Side: "provides", Protocol: "http", Method: "POST", Path: "/orders", Raw: `@app.post("/orders")`})
	}
	if l := lineOfText(content, "fetch("); l > 0 {
		out = append(out, rawEntry{File: file, Line: l, Side: "consumes", Protocol: "http", Method: "POST", Path: "/orders", Base: "ORDERS_URL", Raw: "`${ORDERS_URL}/orders`"})
	}
	if l := lineOfText(content, "requests.get"); l > 0 {
		out = append(out, rawEntry{File: file, Line: l, Side: "consumes", Protocol: "http", Method: "GET", Path: "/users/{id}", Raw: "f'{USERS}/users/{id}'"})
	}
	return out
}

type scanEnv struct {
	t     *testing.T
	st    *memStore
	snaps *fakeSnapshots
	run   *extractRunner
	m     *Map
	now   time.Time
	attr  []string
}

func newScanEnv(t *testing.T, maxRuns int) *scanEnv {
	e := &scanEnv{t: t, st: newMemStore(), snaps: &fakeSnapshots{}, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	e.run = &extractRunner{answer: answerByContent}
	e.m = New(Options{
		Store: e.st, Snapshots: e.snaps, Runner: e.run, Model: "haiku", ScratchDir: t.TempDir(),
		MaxRunsPerHour: maxRuns, MaxFileBytes: 4 << 10,
		Attribute: func(ctx context.Context, repo string) context.Context {
			e.attr = append(e.attr, repo)
			return context.WithValue(ctx, kindKey{}, "map")
		},
		Now: func() time.Time { return e.now },
	})
	return e
}

func (e *scanEnv) snapshot(name, sha string, files map[string]string) {
	dir := e.t.TempDir()
	writeTree(e.t, dir, files)
	for i, d := range e.snaps.dirs {
		if d.Name == name {
			e.snaps.dirs[i] = core.ContextDir{HostPath: dir, Name: name, SHA: sha, Branch: "main"}
			return
		}
	}
	e.snaps.dirs = append(e.snaps.dirs, core.ContextDir{HostPath: dir, Name: name, SHA: sha, Branch: "main"})
}

func (e *scanEnv) pass() {
	e.t.Helper()
	if err := e.m.Pass(context.Background()); err != nil {
		e.t.Fatalf("Pass: %v", err)
	}
}

func TestPassExtractsMatchesAndIsIncremental(t *testing.T) {
	e := newScanEnv(t, 20)
	e.snapshot("g/orders", "sha1", map[string]string{
		"src/routes.py":      routesPy,
		"src/util.py":        "def add(a, b):\n    return a + b\n",
		"tests/test_api.py":  "requests.get('/orders')\n",
		"vendor/lib/x.php":   "<?php Route::get('/x');",
		"api/openapi.yaml":   ordersSpec,
		"src/big.js":         "fetch('/x')\n" + strings.Repeat("// pad\n", 1000),
		"README.md":          "fetch('/orders')",
		"src/app.min.js":     "fetch('/a')",
		"src/types.d.ts":     "fetch('/a')",
		".reviewbot.yaml":    "sysmap:\n  exclude: [\"src/legacy/**\"]\n",
		"src/legacy/old.php": "<?php Route::get('/legacy');",
	})
	e.snapshot("g/web", "w1", map[string]string{"src/api.ts": apiTS})
	e.run.onRun = func() {
		if e.snaps.released != e.snaps.acquired {
			t.Error("snapshots must be released before the model runs")
		}
	}
	e.pass()
	if got := e.run.sentFiles(); !reflect.DeepEqual(got, []string{"src/api.ts", "src/routes.py"}) {
		t.Fatalf("sent = %v", got)
	}
	if len(e.run.reqs) != 2 || !e.run.reqs[0].NoTools || e.run.reqs[0].Model != "haiku" || e.run.reqs[0].JSONSchema != ExtractSchema {
		t.Fatalf("run requests: %+v", e.run.reqs)
	}
	if !strings.Contains(e.run.reqs[0].Prompt, "1| ") || !strings.Contains(e.run.reqs[0].SystemPrompt, "UNTRUSTED") {
		t.Error("prompt must number lines and mark files as untrusted")
	}
	if !reflect.DeepEqual(e.run.ctxKind, []string{"map", "map"}) || len(e.attr) != 2 {
		t.Errorf("attribution: %v %v", e.run.ctxKind, e.attr)
	}
	if len(e.st.runs) != 2 || e.st.runs[0].CostUSD != 0.01 || !e.st.runs[0].OK {
		t.Errorf("runs = %+v", e.st.runs)
	}
	st := e.st.repos["g/orders"]
	if st.Status != StatusOK || st.SHA != "sha1" || st.Candidates != 1 || st.Specs != 1 || st.TooLarge != 1 || st.Version != e.m.version() {
		t.Errorf("orders state = %+v", st)
	}
	x := e.m.Index()
	if len(x.Edges) != 1 || x.Edges[0].From != "g/web" || x.Edges[0].To != "g/orders" || x.Edges[0].Counts[HTTP] != 1 {
		t.Fatalf("edges = %+v", x.Edges)
	}
	var specEntries int
	for _, en := range x.Entries {
		if en.Source == SourceOpenAPI {
			specEntries++
		}
	}
	if specEntries != 1 {
		t.Errorf("spec entries = %d", specEntries)
	}

	// Same SHAs: nothing to do.
	e.pass()
	if len(e.run.reqs) != 2 {
		t.Fatalf("unchanged repos re-extracted: %d runs", len(e.run.reqs))
	}

	// New default-branch commit: one file changed, one added, the spec
	// deleted, one unchanged.
	e.snapshot("g/orders", "sha2", map[string]string{
		"src/routes.py": routesPy + "\n# changed\n",
		"src/client.py": "import requests\nrequests.get(f'{USERS}/users/{id}')\n",
		"src/util.py":   "def add(a, b):\n    return a + b\n",
	})
	e.run.sent = nil
	e.pass()
	if got := e.run.sentFiles(); !reflect.DeepEqual(got, []string{"src/client.py", "src/routes.py"}) {
		t.Fatalf("incremental sent = %v", got)
	}
	if _, ok := e.st.files["g/orders"]["api/openapi.yaml"]; ok {
		t.Error("deleted spec must be dropped")
	}
	if e.st.repos["g/orders"].SHA != "sha2" {
		t.Error("sha not updated")
	}
	// The new call has no provider: unresolved, kept with its raw expression.
	var unresolved []IndexedEntry
	for _, en := range e.m.Index().Entries {
		if en.Status == Unresolved {
			unresolved = append(unresolved, en)
		}
	}
	if len(unresolved) != 1 || unresolved[0].Raw != "f'{USERS}/users/{id}'" || unresolved[0].Reason == "" {
		t.Errorf("unresolved = %+v", unresolved)
	}
}

func TestPassBudgetCarriesOver(t *testing.T) {
	e := newScanEnv(t, 1)
	e.snapshot("g/a", "a1", map[string]string{"x.py": routesPy})
	e.snapshot("g/b", "b1", map[string]string{"y.ts": apiTS})
	e.pass()
	if len(e.run.reqs) != 1 {
		t.Fatalf("runs = %d, want 1 (budget)", len(e.run.reqs))
	}
	var partial string
	for _, r := range e.st.repos {
		if r.Status == StatusPartial {
			partial = r.Path
			if r.Pending != 1 {
				t.Errorf("pending = %d", r.Pending)
			}
		}
	}
	if partial == "" {
		t.Fatalf("one repo must wait for the budget: %+v", e.st.repos)
	}
	// Same hour: still no budget.
	e.pass()
	if len(e.run.reqs) != 1 {
		t.Fatalf("budget exceeded: %d runs", len(e.run.reqs))
	}
	e.now = e.now.Add(61 * time.Minute)
	e.pass()
	if len(e.run.reqs) != 2 || e.st.repos[partial].Status != StatusOK {
		t.Fatalf("partial repo not finished: runs %d, state %+v", len(e.run.reqs), e.st.repos[partial])
	}
}

func TestPassNoCodeBudgetMapsOnlySpecs(t *testing.T) {
	e := newScanEnv(t, 0)
	e.snapshot("g/a", "a1", map[string]string{"x.py": routesPy, "openapi.yaml": ordersSpec})
	e.pass()
	if len(e.run.reqs) != 0 {
		t.Fatal("max_runs_per_hour 0 must not run the model")
	}
	if st := e.st.repos["g/a"]; st.Status != StatusOK || st.Specs != 1 {
		t.Errorf("state = %+v", st)
	}
	if len(e.m.Index().Entries) != 1 {
		t.Errorf("entries = %+v", e.m.Index().Entries)
	}
}

func TestPassRunErrorsAndBadAnswers(t *testing.T) {
	e := newScanEnv(t, 20)
	e.snapshot("g/a", "a1", map[string]string{"x.py": routesPy})
	e.run.err = errors.New("boom")
	e.pass()
	if st := e.st.repos["g/a"]; st.Status != StatusPartial || st.Pending != 1 || st.Error == "" {
		t.Fatalf("after run error: %+v", st)
	}
	if len(e.st.runs) != 1 || e.st.runs[0].OK {
		t.Errorf("failed run not recorded: %+v", e.st.runs)
	}
	// Retried on the next pass; this time the answer is not JSON.
	e.run.err, e.run.output = nil, "not json"
	e.pass()
	if len(e.run.reqs) != 2 {
		t.Fatalf("not retried: %d", len(e.run.reqs))
	}
	f := e.st.files["g/a"]["x.py"]
	if f.Error == "" || len(f.Entries) != 0 || e.st.repos["g/a"].Status != StatusOK {
		t.Fatalf("bad answer: file %+v state %+v", f, e.st.repos["g/a"])
	}
	// New commit, same content: the broken file is not re-sent.
	e.snapshot("g/a", "a2", map[string]string{"x.py": routesPy})
	e.pass()
	if len(e.run.reqs) != 2 {
		t.Errorf("unchanged file with a bad answer re-sent")
	}
}

func TestPassRateLimitStops(t *testing.T) {
	e := newScanEnv(t, 20)
	e.snapshot("g/a", "a1", map[string]string{"x.py": routesPy})
	e.snapshot("g/b", "b1", map[string]string{"y.ts": apiTS})
	e.run.err = &core.ErrClaudeRateLimited{Message: "limit"}
	e.pass()
	if len(e.run.reqs) != 1 {
		t.Fatalf("runs after a rate limit = %d, want 1", len(e.run.reqs))
	}
}

func TestPassExcludedRepoDropped(t *testing.T) {
	e := newScanEnv(t, 20)
	e.snapshot("g/a", "a1", map[string]string{"x.py": routesPy})
	e.pass()
	if _, ok := e.st.repos["g/a"]; !ok {
		t.Fatal("repo not stored")
	}
	e.m.o.Exclude = func(r string) bool { return r == "g/a" }
	e.pass()
	if _, ok := e.st.repos["g/a"]; ok {
		t.Error("excluded repo kept")
	}
	if len(e.m.Index().Entries) != 0 {
		t.Error("excluded repo still in the index")
	}
}

func TestBatches(t *testing.T) {
	mk := func(n int) fileContent { return fileContent{text: strings.Repeat("x", n)} }
	bs := batches([]fileContent{mk(40), mk(40), mk(40), mk(200), mk(10)}, 100, 10)
	var sizes []int
	for _, b := range bs {
		sizes = append(sizes, len(b))
	}
	if !reflect.DeepEqual(sizes, []int{2, 1, 1, 1}) {
		t.Errorf("batch sizes = %v", sizes)
	}
	bs = batches([]fileContent{mk(1), mk(1), mk(1)}, 100, 2)
	if len(bs) != 2 {
		t.Errorf("max files per batch ignored: %d", len(bs))
	}
}

func TestPassDropsStaleRepos(t *testing.T) {
	e := newScanEnv(t, 20)
	e.snapshot("g/a", "a1", map[string]string{"x.py": routesPy})
	e.snapshot("g/b", "b1", map[string]string{"y.ts": apiTS})
	e.pass()
	if len(e.st.repos) != 2 {
		t.Fatalf("repos = %v", e.st.repos)
	}
	// g/b disappears (archived or deleted).
	e.snaps.dirs = e.snaps.dirs[:1]
	e.pass()
	e.now = e.now.Add(23 * time.Hour)
	e.pass()
	if _, ok := e.st.repos["g/b"]; !ok {
		t.Fatal("dropped too early")
	}
	// No snapshots at all (provider trouble) does not count as missing.
	saved := e.snaps.dirs
	e.snaps.dirs = nil
	e.now = e.now.Add(2 * time.Hour)
	e.pass()
	if _, ok := e.st.repos["g/a"]; !ok {
		t.Fatal("an empty acquisition must not drop repositories")
	}
	e.snaps.dirs = saved
	e.pass()
	if _, ok := e.st.repos["g/b"]; ok {
		t.Error("stale repo kept")
	}
	if _, ok := e.st.repos["g/a"]; !ok {
		t.Error("live repo dropped")
	}
}
