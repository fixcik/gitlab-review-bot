package sysmap

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func routes(path string) string {
	return "from app import app\n\n@app.post(\"" + path + "\")\ndef h():\n    return {}\n"
}

func TestDepsBudgetOffSwitches(t *testing.T) {
	f, added := webFile("fetch(a) // CALL GET /orders/{id}/history ORDERS_URL")
	for _, c := range []struct{ scan, deps int }{{0, 5}, {5, 0}} {
		e := newDepsEnv(t, 5)
		e.m.o.MaxRunsPerHour, e.m.o.DepsMaxRunsPerHour = c.scan, c.deps
		res := e.deps(DepsRequest{Files: []DepsFile{f}, Added: added})
		if res.Runs != 0 || len(e.run.reqs) != 0 || res.SkipReason != "code extraction off" || res.Skipped != 1 {
			t.Errorf("max_runs %d, deps_max_runs %d: %+v", c.scan, c.deps, res)
		}
	}
}

func TestPassReparsesChangedSpec(t *testing.T) {
	e := newScanEnv(t, 0)
	e.snapshot("g/a", "a1", map[string]string{"openapi.yaml": ordersSpec, "x.py": routesPy})
	e.pass()
	if st := e.st.repos["g/a"]; st.Candidates != 0 || st.Status != StatusOK {
		t.Errorf("no code budget: no code candidates: %+v", st)
	}
	e.snapshot("g/a", "a2", map[string]string{"openapi.yaml": ordersSpec + "  /orders:\n    post: {}\n"})
	e.pass()
	if n := len(e.st.files["g/a"]["openapi.yaml"].Entries); n != 2 {
		t.Errorf("changed spec not re-parsed: %d entries", n)
	}
	e.snapshot("g/a", "a3", map[string]string{"openapi.yaml": "openapi: 3.0.0\npaths:\n  /x: [unclosed\n"})
	e.pass()
	if f := e.st.files["g/a"]["openapi.yaml"]; f.Error == "" || e.m.Index().FileErrors["g/a"] != 1 {
		t.Errorf("unparsable spec must be recorded with an error: %+v", f)
	}
}

func TestPassCharBudgetKeepsWaitingFiles(t *testing.T) {
	e := newScanEnv(t, 2)
	e.snapshot("g/a", "a1", map[string]string{"a.py": routes("/a"), "b.py": routes("/b"), "c.py": routes("/c"), "d.py": routes("/d")})
	e.pass()
	if len(e.run.reqs) != 1 || len(e.st.files["g/a"]) != 4 {
		t.Fatalf("first pass: %d runs, files %v", len(e.run.reqs), e.st.files["g/a"])
	}
	// One run left: only the first changed file is read, the other two
	// wait; d.py is unchanged.
	e.m.o.MaxBatchChars = 50
	e.snapshot("g/a", "a2", map[string]string{"a.py": routes("/a2"), "b.py": routes("/b2"), "c.py": routes("/c2"), "d.py": routes("/d")})
	e.run.sent = nil
	e.pass()
	if got := e.run.sentFiles(); strings.Join(got, ",") != "a.py" {
		t.Errorf("sent = %v", got)
	}
	st := e.st.repos["g/a"]
	if st.Status != StatusPartial || st.Pending != 2 {
		t.Errorf("state = %+v", st)
	}
	for _, p := range []string{"a.py", "b.py", "c.py", "d.py"} {
		if _, ok := e.st.files["g/a"][p]; !ok {
			t.Errorf("%s dropped from the map", p)
		}
	}
	if es := e.st.files["g/a"]["d.py"].Entries; len(es) != 1 {
		t.Errorf("unchanged file lost its entries: %+v", es)
	}
}

func TestPassRunBudgetCountsRemainingBatches(t *testing.T) {
	e := newScanEnv(t, 1)
	e.m.o.MaxBatchFiles = 1
	var scans []string
	e.m.o.OnScan = func(repo, status string) { scans = append(scans, repo+" "+status) }
	e.snapshot("g/a", "a1", map[string]string{"a.py": routes("/a"), "b.py": routes("/b"), "c.py": routes("/c")})
	e.pass()
	if st := e.st.repos["g/a"]; len(e.run.reqs) != 1 || st.Status != StatusPartial || st.Pending != 2 || st.Candidates != 3 {
		t.Errorf("runs %d, state %+v", len(e.run.reqs), st)
	}
	if strings.Join(scans, ",") != "g/a partial" {
		t.Errorf("OnScan calls: %v", scans)
	}
}

// Settings that choose the candidate files (max_file_kb,
// max_files_per_repo) are part of the repository version: changing them
// re-plans an unchanged SHA, and only the newly selected files go to the
// model. A state stored before the settings were part of the version is
// re-planned once without model runs for the files it already has.
func TestPassReplansWhenFileSelectionChanges(t *testing.T) {
	e := newScanEnv(t, 20)
	e.snapshot("g/web", "w1", map[string]string{
		"src/api.ts": apiTS,
		"src/big.ts": apiTS + strings.Repeat("// pad\n", 1000),
	})
	e.pass()
	if got := e.run.sentFiles(); !reflect.DeepEqual(got, []string{"src/api.ts"}) || e.st.repos["g/web"].TooLarge != 1 {
		t.Fatalf("sent = %v, state %+v", got, e.st.repos["g/web"])
	}

	// A state of an older build: no selection settings in the version.
	st := e.st.repos["g/web"]
	st.Version = SpecVersion + "+" + CodeVersion
	e.st.repos["g/web"] = st
	e.run.sent = nil
	e.pass()
	if len(e.run.sent) != 0 || e.st.repos["g/web"].Version != e.m.version() || e.st.repos["g/web"].Status != StatusOK {
		t.Fatalf("old state: sent %v, state %+v", e.run.sentFiles(), e.st.repos["g/web"])
	}

	// Same settings: nothing to do.
	at := e.st.repos["g/web"].ScannedAt
	e.now = e.now.Add(time.Hour)
	e.pass()
	if !e.st.repos["g/web"].ScannedAt.Equal(at) {
		t.Error("unchanged repository re-planned")
	}

	// max_file_kb raised: the large file is extracted now.
	e.m.o.MaxFileBytes = 64 << 10
	e.pass()
	if got := e.run.sentFiles(); !reflect.DeepEqual(got, []string{"src/big.ts"}) || e.st.repos["g/web"].TooLarge != 0 {
		t.Fatalf("after max_file_kb: sent %v, state %+v", got, e.st.repos["g/web"])
	}

	// max_files_per_repo changes the version too.
	v := e.m.version()
	e.m.o.MaxFilesPerRepo = 1
	if e.m.version() == v {
		t.Error("max_files_per_repo must be part of the version")
	}
}

// Files extracted by an older prompt are extracted again once: code-2
// added the external marks, code-3 the literal event names.
func TestPassReextractsCode1Files(t *testing.T) {
	if CodeVersion != "code-3" {
		t.Fatalf("CodeVersion = %s", CodeVersion)
	}
	e := newScanEnv(t, 20)
	e.snapshot("g/web", "w1", map[string]string{"src/api.ts": apiTS})
	e.pass()
	st := e.st.repos["g/web"]
	st.Version = SpecVersion + "+code-2"
	e.st.repos["g/web"] = st
	f := e.st.files["g/web"]["src/api.ts"]
	f.Version = "code-2"
	e.st.files["g/web"]["src/api.ts"] = f
	e.run.sent = nil
	e.pass()
	if got := e.run.sentFiles(); !reflect.DeepEqual(got, []string{"src/api.ts"}) || e.st.files["g/web"]["src/api.ts"].Version != CodeVersion {
		t.Fatalf("code-1 file: sent %v, record %+v", got, e.st.files["g/web"]["src/api.ts"])
	}
}

func TestLoadUsesGitLabDomain(t *testing.T) {
	e := newScanEnv(t, 20)
	e.m.o.GitLabURL = "https://gitlab.team.ru"
	e.snapshot("g/web", "w1", map[string]string{"src/api.ts": "fetch('https://crm.team.ru/x')\n"})
	e.run.answer = func(file, _ string) []rawEntry {
		return []rawEntry{{File: file, Line: 1, Side: "consumes", Protocol: "http", Method: "GET", Path: "/x", Raw: "https://crm.team.ru/x", ExternalHost: "crm.team.ru"}}
	}
	e.pass()
	if en := e.m.Index().Entries; len(en) != 1 || en[0].Status != Unresolved {
		t.Errorf("entries = %+v", en)
	}
}
