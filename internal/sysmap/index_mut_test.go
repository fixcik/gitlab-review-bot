package sysmap

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildCountsFileErrorsAndSortsEntries(t *testing.T) {
	files := map[string][]FileRecord{}
	for _, r := range []string{"g/e", "g/c", "g/a", "g/d", "g/b"} {
		files[r] = []FileRecord{fr("x.py", prov("GET", "/"+strings.TrimPrefix(r, "g/")+"/thing"))}
	}
	files["g/c"] = append(files["g/c"], FileRecord{Path: "bad.py", Error: "unusable answer"})
	x := Build(nil, files, nil, time.Now())
	if len(x.FileErrors) != 1 || x.FileErrors["g/c"] != 1 {
		t.Errorf("file errors = %v", x.FileErrors)
	}
	var repos []string
	for _, e := range x.Entries {
		repos = append(repos, e.Repo)
	}
	if strings.Join(repos, ",") != "g/a,g/b,g/c,g/d,g/e" {
		t.Errorf("entries must be in repository order: %v", repos)
	}
}

func TestBuildEdgesSorted(t *testing.T) {
	files := map[string][]FileRecord{}
	var calls []Entry
	for _, to := range []string{"g/p1", "g/p2", "g/p3", "g/p4", "g/p5"} {
		files[to] = []FileRecord{fr("r.py", prov("GET", "/"+strings.TrimPrefix(to, "g/")+"/x"))}
		calls = append(calls, cons("GET", "/"+strings.TrimPrefix(to, "g/")+"/x", ""))
	}
	files["g/b"] = []FileRecord{fr("b.ts", calls...)}
	files["g/a"] = []FileRecord{fr("a.ts", calls[4], calls[0])}
	x := Build(nil, files, nil, time.Now())
	var got []string
	for _, ed := range x.Edges {
		got = append(got, ed.From+">"+ed.To)
	}
	want := "g/a>g/p1 g/a>g/p5 g/b>g/p1 g/b>g/p2 g/b>g/p3 g/b>g/p4 g/b>g/p5"
	if strings.Join(got, " ") != want {
		t.Errorf("edges = %v\nwant %s", got, want)
	}
}

func TestBuildEventConsumers(t *testing.T) {
	files := map[string][]FileRecord{
		"g/srv": {fr("s.py",
			prov("GET", "/feed"),
			Entry{Side: Provides, Protocol: SSE, Path: "/events", Event: "order.updated"},
			Entry{Side: Provides, Protocol: WS, Event: "update"})},
		"g/web": {fr("w.ts",
			Entry{Side: Consumes, Protocol: SSE, Path: "/events"},
			Entry{Side: Consumes, Protocol: SSE, Event: "order.updated"},              // pair
			Entry{Side: Consumes, Protocol: WS, Event: "update"},                      // generic, one provider
			Entry{Side: Consumes, Protocol: WS, Event: "chat.x", Base: "CHAT_WS"},     // hint
			Entry{Side: Consumes, Protocol: WS, Event: "self.evt", Base: "SELF_WS"})}, // hint to itself
	}
	x := Build(nil, files, map[string]string{"CHAT_WS": "g/chat", "SELF_WS": "g/web/"}, time.Now())
	if e := find(x, "g/web", "order.updated"); e.Status != Resolved || e.Via != ViaPair || e.Reason != "" {
		t.Errorf("paired event: %+v", e)
	}
	if e := find(x, "g/web", "update"); e.Status != Unresolved || e.Reason != "общее имя события, нет связи по пути" || len(e.Candidates) != 1 {
		t.Errorf("generic event: %+v", e)
	}
	if e := find(x, "g/web", "chat.x"); e.Status != Resolved || e.Target != "g/chat" || e.Via != ViaHint {
		t.Errorf("event hint: %+v", e)
	}
	if e := find(x, "g/web", "self.evt"); e.Status != Internal || e.Target != "g/web" || e.Via != ViaHint {
		t.Errorf("event hint to itself: %+v", e)
	}
	if !specificEvent("a.b.c") || specificEvent("a.bc") || specificEvent("plainword") {
		t.Error("specificEvent: a 5+ character name with a separator is specific")
	}
}

func TestNormalizeMut(t *testing.T) {
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE"} {
		if NormalizeMethod(strings.ToLower(m)) != m {
			t.Errorf("NormalizeMethod(%q)", m)
		}
	}
	for in, want := range map[string]string{
		"/orders/${order.item.id}": "/orders/{id}",
		"user_profiles/1":          "/user_profiles/1",
		"V2/orders":                "/V2/orders",
		"ORDERS_URL/orders":        "/orders",
	} {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
	if PathKey("") != "" {
		t.Error(`PathKey("") must stay empty`)
	}
	for _, bad := range []string{"has space", "quote\"d", "<script>", strings.Repeat("a", maxEventRunes+1)} {
		if NormalizeEvent(bad) != "" {
			t.Errorf("NormalizeEvent(%q) accepted", bad)
		}
	}
	if got := clip("abcdef", 3); got != "abc…" {
		t.Errorf("clip = %q", got)
	}
	if got := clip(" a  b\n c ", 10); got != "a b c" {
		t.Errorf("clip one line = %q", got)
	}
}

func TestParseExtractionValidation(t *testing.T) {
	batch := []fileContent{{candidate: candidate{Path: "a.ts"}, lines: 5}}
	out := `{"entries": [
	 {"file": "a.ts", "line": 0, "side": "consumes", "protocol": "http", "method": "GET", "path": "/zero"},
	 {"file": "a.ts", "line": 1, "side": "consumes", "protocol": "ws", "path": "", "event": "", "raw": "socket"},
	 {"file": "a.ts", "line": 1, "side": "consumes", "protocol": "http", "method": "GET", "path": "", "event": "order.paid", "raw": "x"},
	 {"file": "a.ts", "line": 2, "end_line": 9, "side": "consumes", "protocol": "http", "method": "GET", "path": "", "raw": "${API}/orders/${id}"},
	 {"file": "a.ts", "line": 2, "side": "consumes", "protocol": "ws", "path": "", "event": "chat.message", "raw": "wss://chat/socket"},
	 {"file": "a.ts", "line": 3, "end_line": 5, "side": "provides", "protocol": "http", "method": "GET", "path": "/ok"}
	]}`
	res, err := parseExtraction(out, batch)
	if err != nil {
		t.Fatal(err)
	}
	a := res["a.ts"]
	if len(a) != 3 {
		t.Fatalf("entries = %+v", a)
	}
	if a[1].Protocol != WS || a[1].Path != "" || a[1].Event != "chat.message" {
		t.Errorf("event entries take no path from raw: %+v", a[1])
	}
	a = append(a[:1], a[2:]...)
	if a[0].Line != 2 || a[0].Path != "/orders/{id}" || a[0].EndLine != 0 {
		t.Errorf("path from raw, end_line past the file ignored: %+v", a[0])
	}
	if a[1].Path != "/ok" || a[1].EndLine != 5 {
		t.Errorf("entries after dropped ones must be kept: %+v", a[1])
	}
}

func TestBatchesDefaultsAndBoundary(t *testing.T) {
	mk := func(n int) fileContent { return fileContent{text: strings.Repeat("x", n)} }
	var many []fileContent
	for range 30 {
		many = append(many, mk(10))
	}
	if bs := batches(many, 0, 0); len(bs) != 2 || len(bs[0]) != defaultBatchFiles {
		t.Errorf("defaults: %d batches", len(bs))
	}
	if bs := batches([]fileContent{mk(50), mk(50)}, 100, 10); len(bs) != 1 {
		t.Errorf("files filling maxChars exactly share a batch: %d", len(bs))
	}
}

func TestParseSpecMethodsRefAndCap(t *testing.T) {
	spec := "openapi: 3.0.0\npaths:\n  /a:\n    patch: {}\n    trace: {}\n  /b:\n    $ref: '#/x'\n    get: {}\n"
	es, err := ParseSpec("o.yaml", []byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range es {
		got = append(got, e.Method+" "+e.Path)
	}
	if strings.Join(got, ",") != "PATCH /a,TRACE /a,GET /b" {
		t.Errorf("entries = %v", got)
	}
	var b strings.Builder
	b.WriteString("openapi: 3.0.0\npaths:\n")
	for i := range maxSpecEntries + 5 {
		fmt.Fprintf(&b, "  /p%d:\n    get: {}\n", i)
	}
	es, err = ParseSpec("big.yaml", []byte(b.String()))
	if err != nil || len(es) != maxSpecEntries {
		t.Errorf("cap: %d entries, err %v", len(es), err)
	}
}
