package compat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// faultCheckout is memCheckout with injected failures.
type faultCheckout struct {
	*memCheckout
	changedErr error
	listErr    map[string]error // by rev
	// showOK: "rev:path" -> how many ShowFile calls succeed before it
	// fails with showErr.
	showOK  map[string]int
	showErr error
	shows   map[string]int
	// list: extra files ListFiles reports by rev (not readable).
	list map[string][]string
}

func (c *faultCheckout) ChangedFiles(ctx context.Context, base string, ign []string) ([]string, error) {
	if c.changedErr != nil {
		return nil, c.changedErr
	}
	return c.memCheckout.ChangedFiles(ctx, base, ign)
}

func (c *faultCheckout) ListFiles(ctx context.Context, rev string) ([]string, error) {
	if err := c.listErr[rev]; err != nil {
		return nil, err
	}
	l, err := c.memCheckout.ListFiles(ctx, rev)
	return append(l, c.list[rev]...), err
}

func (c *faultCheckout) ShowFile(ctx context.Context, rev, p string) ([]byte, error) {
	k := rev + ":" + p
	if n, ok := c.showOK[k]; ok {
		if c.shows == nil {
			c.shows = map[string]int{}
		}
		c.shows[k]++
		if c.shows[k] > n {
			return nil, c.showErr
		}
	}
	return c.memCheckout.ShowFile(ctx, rev, p)
}

// scratchCheckout hands out a scratch dir outside the clone (repo.Checkout
// with the checkout cache).
type scratchCheckout struct {
	*memCheckout
	dir string
	err error
}

func (c *scratchCheckout) ScratchDir(name string) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	d := filepath.Join(c.dir, name)
	return d, os.MkdirAll(d, 0o755)
}

func newMat(t *testing.T, co core.Checkout) *materializer {
	t.Helper()
	m, err := newMaterializer(co, "base", "head")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.cleanup)
	return m
}

func onlyIssue(t *testing.T, res *Result, source, kind, reason string) {
	t.Helper()
	if len(res.Issues) != 1 || res.Issues[0].Source != source || res.Issues[0].Kind != kind || !strings.Contains(res.Issues[0].Reason, reason) {
		t.Fatalf("issues %+v, want one %s/%s %q", res.Issues, source, kind, reason)
	}
	if len(res.Changes) != 0 || res.SourceOK(source) == (kind != IssueSkipped) {
		t.Fatalf("changes %+v, SourceOK %v", res.Changes, res.SourceOK(source))
	}
}

func bothSides(co *memCheckout, p, content string) {
	co.trees["base"][p] = content
	co.trees["head"][p] = content
}

// The checks cannot start: an error, not an empty result (nothing may be
// auto-resolved on it).
func TestCheckCannotStart(t *testing.T) {
	co := newCheckout(t)
	fc := &faultCheckout{memCheckout: co, changedErr: errors.New("git: bad revision")}
	if res, err := (&Checker{Runner: &scriptRunner{}}).Check(context.Background(), Request{Checkout: fc, Base: "base", Head: "head"}); err == nil || res != nil ||
		!strings.Contains(err.Error(), "compat: changed files: git: bad revision") {
		t.Fatalf("%+v %v", res, err)
	}

	// The scratch dir cannot be prepared.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := &memCheckout{dir: file, trees: map[string]map[string]string{"base": {}, "head": {}}, changed: []string{"openapi.yaml"}}
	bothSides(bad, "openapi.yaml", "openapi: 3.0.0\n")
	r := &scriptRunner{}
	if res, err := (&Checker{Runner: r}).Check(context.Background(), Request{Checkout: bad, Base: "base", Head: "head"}); err == nil || res != nil || len(r.reqs) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	var nilRes *Result
	if nilRes.SourceOK(SourceOasdiff) {
		t.Error("no result: no source is ok")
	}
}

// Spec discovery sniffs a deleted file from the base and does not sniff
// huge files.
func TestCheckSniffing(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"svc/api.yaml", "svc/big.yaml"}
	co.trees["base"]["svc/api.yaml"] = "openapi: 3.0.0\n"
	bothSides(co, "svc/big.yaml", "openapi: 3.0.0\n"+strings.Repeat("#\n", maxSniffBytes/2+1))
	r := &scriptRunner{}
	res := check(t, co, r, Config{})
	if len(res.Deleted) != 1 || res.Deleted[0].Path != "svc/api.yaml" || len(res.Specs) != 0 || len(r.reqs) != 0 {
		t.Fatalf("res %+v", res)
	}
}

// A checkout with its own scratch dir: the files go there, the clone is
// untouched; failures to get or prepare it fail the check.
func TestMaterializerScratchDir(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"openapi.yaml"}
	bothSides(co, "openapi.yaml", "openapi: 3.0.0\n")
	sc := &scratchCheckout{memCheckout: co, dir: t.TempDir()}
	r := &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {Stdout: "[]"}}}
	res, err := (&Checker{Runner: r}).Check(context.Background(), Request{Checkout: sc, Base: "base", Head: "head"})
	if err != nil || len(res.Issues) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(r.reqs) != 1 || r.reqs[0].Dir != filepath.Join(sc.dir, ScratchDir) || r.seen[0]["new/openapi.yaml"] == "" {
		t.Fatalf("reqs %+v seen %v", r.reqs, r.seen)
	}
	if _, err := os.Stat(filepath.Join(co.dir, ScratchDir)); !os.IsNotExist(err) {
		t.Errorf("clone touched: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(co.dir, ".git", "info", "exclude")); len(b) != 0 {
		t.Errorf("exclude written for a scratch dir outside the clone: %q", b)
	}
	if _, err := os.Stat(filepath.Join(sc.dir, ScratchDir)); !os.IsNotExist(err) {
		t.Errorf("scratch dir not removed: %v", err)
	}

	sc.err = errors.New("no space")
	if _, err := newMaterializer(sc, "base", "head"); err == nil || !strings.Contains(err.Error(), "compat: scratch dir: no space") {
		t.Errorf("ScratchDir error: %v", err)
	}
	// A side directory cannot be created.
	sc.err = nil
	if err := os.MkdirAll(filepath.Join(sc.dir, ScratchDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sc.dir, ScratchDir, "old"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newMaterializer(sc, "base", "head"); err == nil || !strings.Contains(err.Error(), "compat: scratch dir") {
		t.Errorf("side dir error: %v", err)
	}
}

// write: once per file and side, unsafe paths refused, read errors passed
// on, the per-side limits enforced.
func TestMaterializerWrite(t *testing.T) {
	co := newCheckout(t)
	co.trees["head"]["a.yaml"] = "one"
	fc := &faultCheckout{memCheckout: co, showOK: map[string]int{"head:a.yaml": 1, "head:b.yaml": 0}, showErr: errors.New("git cat-file: broken pipe")}
	m := newMat(t, fc)
	ctx := context.Background()
	for i := 0; i < 2; i++ { // the second write is served from the cache
		if err := m.write(ctx, "new", "a.yaml"); err != nil {
			t.Fatal(err)
		}
	}
	if fc.shows["head:a.yaml"] != 1 || m.count["new"] != 1 || m.bytes["new"] != 3 {
		t.Errorf("shows %v count %v bytes %v", fc.shows, m.count, m.bytes)
	}
	if err := m.write(ctx, "new", "../etc/passwd"); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Errorf("unsafe: %v", err)
	}
	if err := m.write(ctx, "new", "b.yaml"); err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Errorf("read error: %v", err)
	}
	if err := m.write(ctx, "new", "missing.yaml"); err != nil {
		t.Errorf("a missing file is skipped: %v", err)
	}
	co.trees["head"]["c.yaml"] = "x"
	m.count["new"] = maxFilesPerSide
	if err := m.write(ctx, "new", "c.yaml"); !errors.Is(err, errTooMany) {
		t.Errorf("file limit: %v", err)
	}
	m.count["new"], m.bytes["new"] = 0, maxBytesPerSide
	if err := m.write(ctx, "new", "c.yaml"); !errors.Is(err, errTooMany) {
		t.Errorf("byte limit: %v", err)
	}
	// The target is a directory: the write fails.
	m.bytes["new"] = 0
	co.trees["head"]["d"] = "x"
	if err := os.MkdirAll(filepath.Join(m.dir, "new", "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.write(ctx, "new", "d"); err == nil || m.written["new/d"] {
		t.Errorf("write over a directory: %v", err)
	}
}

func openAPICheck(t *testing.T, co core.Checkout, spec string, r *scriptRunner) *Result {
	t.Helper()
	res := &Result{}
	(&Checker{Runner: r}).checkOpenAPI(context.Background(), newMat(t, co), spec, "ru", res)
	return res
}

// Every way the OpenAPI check can fail is an Issue (the spec counts as not
// checked), never "no changes".
func TestCheckOpenAPIFailures(t *testing.T) {
	spec := "api/openapi.yaml"
	plain := func() *memCheckout {
		co := newCheckout(t)
		bothSides(co, spec, "openapi: 3.0.0\n")
		return co
	}
	oas := func(a core.ExecResult) *scriptRunner {
		return &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": a}}
	}

	r := oas(core.ExecResult{})
	onlyIssue(t, openAPICheck(t, plain(), "api/../openapi.yaml", r), SourceOasdiff, IssueFailed, "не удалось подготовить файлы: unsafe path")
	if len(r.reqs) != 0 {
		t.Error("oasdiff ran")
	}

	co := plain()
	co.trees["head"][spec] = "openapi: 3.0.0\npaths:\n  /x:\n    $ref: 'https://evil.example/paths.yaml'\n"
	r = oas(core.ExecResult{})
	onlyIssue(t, openAPICheck(t, co, spec, r), SourceOasdiff, IssueFailed, "по URL или абсолютному пути (https://evil.example/paths.yaml)")
	if len(r.reqs) != 0 {
		t.Error("oasdiff ran with a URL $ref")
	}

	for _, tc := range []struct {
		name   string
		answer core.ExecResult
		reason string
	}{
		{"refused ref", core.ExecResult{ExitCode: 102, Stderr: "Error: disallowed external reference: http://x"}, "загрузка внешних ссылок отключена: Error: disallowed external reference"},
		{"bad flags", core.ExecResult{ExitCode: 101, Stderr: "unknown flag"}, "неверные флаги oasdiff: unknown flag"},
		{"external ref", core.ExecResult{ExitCode: 123}, "спецификация ссылается на внешний $ref"},
		{"other code", core.ExecResult{ExitCode: 7, Stdout: "boom"}, "oasdiff завершился с кодом 7: boom"},
		{"garbage", core.ExecResult{Stdout: "not json"}, "непонятный вывод oasdiff: not json"},
		{"exit 1 without errors", core.ExecResult{ExitCode: 1, Stdout: "[]"}, "oasdiff завершился с ошибкой"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			onlyIssue(t, openAPICheck(t, plain(), spec, oas(tc.answer)), SourceOasdiff, IssueFailed, tc.reason)
		})
	}

	// The spec cannot be read for its version.
	fc := &faultCheckout{memCheckout: plain(), showOK: map[string]int{"base:" + spec: 1}, showErr: errors.New("i/o timeout")}
	r = oas(core.ExecResult{Stdout: "[]"})
	onlyIssue(t, openAPICheck(t, fc, spec, r), SourceOasdiff, IssueFailed, "не удалось прочитать спецификацию: i/o timeout")
	if len(r.reqs) != 0 {
		t.Error("oasdiff ran on an unread side")
	}

	// Swagger other than 2.x.
	co = plain()
	co.trees["base"][spec] = "swagger: '3.0'\n"
	onlyIssue(t, openAPICheck(t, co, spec, oas(core.ExecResult{Stdout: "[]"})), SourceOasdiff, IssueFailed, `неподдерживаемая версия swagger: "3.0"`)

	// The head side has an unsupported version.
	co = plain()
	co.trees["head"][spec] = "swagger: '1.2'\n"
	r = oas(core.ExecResult{Stdout: "[]"})
	onlyIssue(t, openAPICheck(t, co, spec, r), SourceOasdiff, IssueFailed, `неподдерживаемая версия swagger: "1.2"`)
	if len(r.reqs) != 0 {
		t.Error("oasdiff ran")
	}

	// oas2to3 does not recognize the file.
	co = plain()
	co.trees["base"][spec] = "swagger: '2.0'\n"
	r = &scriptRunner{answers: map[string]core.ExecResult{"oas2to3": {ExitCode: 3, Stderr: "not swagger"}, "oasdiff": {Stdout: "[]"}}}
	onlyIssue(t, openAPICheck(t, co, spec, r), SourceOasdiff, IssueFailed, "файл не распознан как Swagger 2.0: not swagger")
	if len(r.reqs) != 1 {
		t.Errorf("oasdiff must not run on an unconverted swagger: %+v", r.reqs)
	}
}

// Sibling files for relative $refs are best effort: without a file list,
// with too many siblings or after a failed copy the spec is still diffed.
func TestWriteOpenAPISiblings(t *testing.T) {
	spec := "api/openapi.yaml"
	base := func() *memCheckout {
		co := newCheckout(t)
		bothSides(co, spec, "openapi: 3.0.0\n")
		bothSides(co, "api/schemas/user.yaml", "type: object\n")
		return co
	}
	run := func(co core.Checkout) map[string]string {
		t.Helper()
		r := &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {Stdout: "[]"}}}
		if res := openAPICheck(t, co, spec, r); len(res.Issues) != 0 || len(r.reqs) != 1 {
			t.Fatalf("res %+v reqs %d", res, len(r.reqs))
		}
		return r.seen[0]
	}

	seen := run(&faultCheckout{memCheckout: base(), listErr: map[string]error{"base": errors.New("ls-tree failed"), "head": errors.New("ls-tree failed")}})
	if seen["old/"+spec] == "" || seen["new/"+spec] == "" {
		t.Errorf("spec not materialized: %v", seen)
	}
	if _, ok := seen["old/api/schemas/user.yaml"]; ok {
		t.Error("siblings without a file list")
	}

	co := base()
	for i := 0; i < openAPINeighbors; i++ {
		co.trees["head"][fmt.Sprintf("api/gen/%04d.json", i)] = "{}"
	}
	seen = run(co)
	if _, ok := seen["new/api/schemas/user.yaml"]; ok || seen["old/api/schemas/user.yaml"] == "" {
		t.Errorf("head has more than %d siblings: none copied; base has one: %v", openAPINeighbors, len(seen))
	}

	// A sibling that cannot be copied stops the copying of the rest.
	co = base()
	co.trees["base"]["api/../escape.yaml"] = "x" // listed before the schemas
	seen = run(co)
	if _, ok := seen["old/api/schemas/user.yaml"]; ok || seen["new/api/schemas/user.yaml"] == "" {
		t.Errorf("seen %v", seen)
	}
}

// Two specs in one directory: the shared files are copied once, both
// specs are diffed.
func TestCheckTwoSpecsOneDir(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"api/openapi-a.yaml", "api/openapi-b.yaml"}
	bothSides(co, "api/openapi-a.yaml", "openapi: 3.0.0\n")
	bothSides(co, "api/openapi-b.yaml", "openapi: 3.0.0\n")
	r := &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {Stdout: "[]"}}}
	res := check(t, co, r, Config{})
	if len(r.reqs) != 2 || len(res.API) != 2 || len(res.Issues) != 0 {
		t.Fatalf("reqs %d res %+v", len(r.reqs), res)
	}
}

func protoCheck(t *testing.T, co core.Checkout, changed []string, r *scriptRunner, prep func(m *materializer)) *Result {
	t.Helper()
	m := newMat(t, co)
	if prep != nil {
		prep(m)
	}
	res := &Result{}
	(&Checker{Runner: r}).checkProto(context.Background(), m, changed, res)
	return res
}

// buf: every failure is an Issue; a file outside the buf modules is
// reported as not checked; a new module has nothing to break.
func TestCheckProtoFailures(t *testing.T) {
	buf := func(a core.ExecResult) *scriptRunner {
		return &scriptRunner{answers: map[string]core.ExecResult{"buf": a}}
	}
	mod := func() *memCheckout {
		co := newCheckout(t)
		bothSides(co, "proto/buf.yaml", "version: v2\n")
		bothSides(co, "proto/a.proto", "syntax = \"proto3\";\n")
		return co
	}

	r := buf(core.ExecResult{})
	onlyIssue(t, protoCheck(t, &faultCheckout{memCheckout: mod(), listErr: map[string]error{"base": errors.New("ls-tree")}}, []string{"proto/a.proto"}, r, nil),
		SourceBuf, IssueFailed, "не удалось получить список файлов: ls-tree")

	r = buf(core.ExecResult{})
	co := mod()
	bothSides(co, "other/b.proto", "syntax = \"proto3\";\n")
	res := protoCheck(t, co, []string{"other/b.proto"}, r, nil)
	onlyIssue(t, res, SourceBuf, IssueSkipped, "файл вне модулей buf")
	if res.Issues[0].Path != "other/b.proto" || len(r.reqs) != 0 {
		t.Errorf("res %+v reqs %d", res, len(r.reqs))
	}

	r = buf(core.ExecResult{})
	onlyIssue(t, protoCheck(t, mod(), []string{"proto/a.proto"}, r, func(m *materializer) { m.count["old"] = maxFilesPerSide }),
		SourceBuf, IssueFailed, "слишком много proto-файлов для проверки")

	fc := &faultCheckout{memCheckout: mod(), list: map[string][]string{"head": {"proto/../x.proto"}}}
	res = protoCheck(t, fc, []string{"proto/a.proto"}, buf(core.ExecResult{}), nil)
	onlyIssue(t, res, SourceBuf, IssueFailed, "не удалось подготовить файлы: unsafe path")
	if res.Issues[0].Path != "proto" {
		t.Errorf("subject %q", res.Issues[0].Path)
	}

	// The module is new: nothing to compare against.
	co = newCheckout(t)
	co.trees["head"]["proto/buf.yaml"] = "version: v2\n"
	co.trees["head"]["proto/a.proto"] = "syntax = \"proto3\";\n"
	r = buf(core.ExecResult{ExitCode: 100, Stdout: bufOut})
	if res := protoCheck(t, co, []string{"proto/a.proto"}, r, nil); len(res.Issues)+len(res.Changes) != 0 || len(r.reqs) != 0 {
		t.Errorf("new module: %+v reqs %d", res, len(r.reqs))
	}

	for _, tc := range []struct {
		name   string
		answer core.ExecResult
		reason string
	}{
		{"exit code", core.ExecResult{ExitCode: 2, Stderr: "Failure: bad config"}, "buf завершился с кодом 2: Failure: bad config"},
		{"no annotations", core.ExecResult{ExitCode: 100}, "buf завершился с ошибкой"},
		{"unparsable", core.ExecResult{ExitCode: 100, Stderr: "panic: oops"}, "buf не смог собрать proto-файлы: panic: oops"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			onlyIssue(t, protoCheck(t, mod(), []string{"proto/a.proto"}, buf(tc.answer), nil), SourceBuf, IssueFailed, tc.reason)
		})
	}
	// No breaking change.
	if res := protoCheck(t, mod(), []string{"proto/a.proto"}, buf(core.ExecResult{}), nil); len(res.Issues)+len(res.Changes) != 0 {
		t.Errorf("clean: %+v", res)
	}
}

func TestCheckGraphQLPrepareAndClean(t *testing.T) {
	co := newCheckout(t)
	r := &scriptRunner{answers: map[string]core.ExecResult{"graphql-inspector": {}}}
	res := &Result{}
	c := &Checker{Runner: r}
	c.checkGraphQL(context.Background(), newMat(t, co), "a/../schema.graphql", res)
	onlyIssue(t, res, SourceGraphQL, IssueFailed, "не удалось подготовить файлы: unsafe path")
	if len(r.reqs) != 0 {
		t.Error("graphql-inspector ran")
	}
	bothSides(co, "schema.graphql", "type Q { a: Int }\n")
	res = &Result{}
	c.checkGraphQL(context.Background(), newMat(t, co), "schema.graphql", res)
	if len(res.Issues)+len(res.Changes) != 0 || len(r.reqs) != 1 {
		t.Errorf("no changes: %+v", res)
	}
}

// Tool runs get the configured timeout; long error texts are cut.
func TestRunTimeoutAndShortText(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"openapi.yaml"}
	bothSides(co, "openapi.yaml", "openapi: 3.0.0\n")
	r := &scriptRunner{errs: map[string]error{"oasdiff": errors.New(strings.Repeat("ж", 400))}}
	res, err := (&Checker{Runner: r, Timeout: 5 * time.Second}).Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head"})
	if err != nil {
		t.Fatal(err)
	}
	if r.reqs[0].Timeout != 5*time.Second {
		t.Errorf("timeout %v", r.reqs[0].Timeout)
	}
	want := "не удалось запустить: " + strings.Repeat("ж", 300) + "…"
	if len(res.Issues) != 1 || res.Issues[0].Reason != want {
		t.Errorf("issues %+v", res.Issues)
	}
	if (&Checker{}).timeout() != defaultTimeout {
		t.Error("default timeout")
	}
}

// oasdiff levels by name; a level below 1 is shown as information and never
// blocks.
func TestParseOasdiffLevels(t *testing.T) {
	out := `[{"id":"a","text":"A","level":"error"},{"id":"b","text":"B","level":"WARNING"},{"id":"c","text":"C","level":"info"},
{"id":"d","text":"D","level":"bogus"},{"id":"e","text":"E","level":0},{"id":"f","text":"F","level":"err"},{"id":"g","text":"G","level":"warn"}]`
	items, errs, warns, err := ParseOasdiffChangelog([]byte(out), "s.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	var lv []int
	for _, it := range items {
		lv = append(lv, it.Level)
	}
	if got := fmt.Sprint(lv); got != "[3 2 1 1 1 3 2]" {
		t.Errorf("levels %s", got)
	}
	if len(errs) != 2 || errs[0].Rule != "a" || errs[1].Rule != "f" || len(warns) != 2 || warns[0].Rule != "b" || warns[1].Rule != "g" {
		t.Errorf("errs %+v warns %+v", errs, warns)
	}
}

// buf lines that are not annotations are skipped; a path outside the
// scratch dir is kept as the repository path.
func TestParseBufOddLines(t *testing.T) {
	out := "{not json\n" + `{"path":"./proto/a.proto","start_line":3,"type":"FIELD_NO_DELETE","message":" gone "}` + "\n" +
		`{"path":"old/proto/b.proto","start_line":9,"type":"FILE_NO_DELETE","message":"b"}`
	changes, compileErr, err := ParseBuf(out)
	if err != nil || compileErr != "" || len(changes) != 2 {
		t.Fatalf("%+v %q %v", changes, compileErr, err)
	}
	if c := changes[0]; c.Path != "proto/a.proto" || c.Line != 0 || c.Message != "gone" || c.Rule != "FIELD_NO_DELETE" {
		t.Errorf("outside scratch: %+v", c)
	}
	if c := changes[1]; c.Path != "proto/b.proto" || c.Line != 0 {
		t.Errorf("old side has no head line: %+v", c)
	}
}

// Configured globs win over the built-in ones; globs match the whole
// path; empty and invalid globs are ignored.
func TestDiscoverConfiguredKinds(t *testing.T) {
	cfg := Config{Proto: []string{"", "idl/*.idl"}, GraphQL: []string{"  ", "**/api.schema"}}
	got := Discover([]string{"idl/a.idl", "svc/api.schema", "web/q.graphql", "x/y.txt"}, cfg, nil)
	want := map[string]string{"idl/a.idl": KindProto, "svc/api.schema": KindGraphQL, "web/q.graphql": KindGraphQL}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, s := range got {
		if want[s.Path] != s.Kind {
			t.Errorf("%s: %s", s.Path, s.Kind)
		}
	}
	if !matchAny([]string{"**/api.schema"}, "api.schema") || matchAny([]string{"api.schema"}, "svc/api.schema") || matchAny([]string{"[", ""}, "[") {
		t.Error("matchAny")
	}
}

func TestSpecVersionJSONEdges(t *testing.T) {
	for in, want := range map[string][2]string{
		`{"openapi": true}`:           {"openapi", ""},
		`{"swagger": 2.0}`:            {"swagger", "2.0"},
		`{"a": }`:                     {"", ""},
		`{"a": 1 "openapi": "3.0.0"}`: {"", ""},
		`{"a": 1`:                     {"", ""},
		`{"info": {}}`:                {"", ""},
	} {
		if k, v := SpecVersion([]byte(in)); k != want[0] || v != want[1] {
			t.Errorf("%s: %q %q, want %v", in, k, v, want)
		}
	}
}

// The $ref scan of a side that was not materialized refuses; $refs to
// files that are missing or not regular files are not followed.
func TestScanRefsUnreadable(t *testing.T) {
	if p, why := scanRefs(filepath.Join(t.TempDir(), "missing"), "a.yaml"); p != refsRefused || why != "не удалось прочитать спецификацию" {
		t.Errorf("%v %q", p, why)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "api", "dir.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := "openapi: 3.0.0\na:\n  $ref: 'missing.yaml'\nb:\n  $ref: 'dir.yaml'\nc:\n  $ref: './'\n"
	if err := os.WriteFile(filepath.Join(dir, "api", "openapi.yaml"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, why := scanRefs(dir, "api/openapi.yaml"); p != refsLocal || why != "" {
		t.Errorf("%v %q", p, why)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"../x", "api/dir.yaml", "api/missing.yaml"} {
		if _, ok := readRoot(root, name); ok {
			t.Errorf("%s read", name)
		}
	}
	if b, ok := readRoot(root, "api/openapi.yaml"); !ok || string(b) != spec {
		t.Error("regular file not read")
	}
}

// A Swagger 2.0 -> OpenAPI 3 migration: the potentially breaking changes
// located in the converted old file are reported on the spec itself.
func TestCheckMigrationWarningPaths(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"api/openapi.yaml"}
	co.trees["base"]["api/openapi.yaml"] = "swagger: '2.0'\ninfo: {title: x, version: '1'}\npaths: {}\n"
	co.trees["head"]["api/openapi.yaml"] = "openapi: 3.0.3\ninfo: {title: x, version: '2'}\npaths: {}\n"
	out := `[{"id":"response-property-became-optional","text":"became optional","level":2,"operation":"GET","path":"/a","baseSource":{"file":"old/api/openapi.yaml.reviewbot-oas3.yaml","line":7}},
	 {"id":"x","text":"other file","level":2,"operation":"GET","path":"/b","baseSource":{"file":"old/api/common.yaml","line":3}}]`
	r := &scriptRunner{answers: map[string]core.ExecResult{"oas2to3": {}, "oasdiff": {Stdout: out}}}
	res := check(t, co, r, Config{})
	if len(res.Warnings) != 2 || res.Warnings[0].Path != "api/openapi.yaml" || res.Warnings[1].Path != "api/common.yaml" || len(res.Changes) != 0 {
		t.Fatalf("warnings %+v", res.Warnings)
	}
	if res.API[0].Items[0].File != "api/openapi.yaml" || !res.API[0].Converted {
		t.Errorf("api %+v", res.API)
	}
}

// Exactly openAPINeighbors siblings are still copied.
func TestWriteOpenAPISiblingsAtTheCap(t *testing.T) {
	co := newCheckout(t)
	bothSides(co, "api/openapi.yaml", "openapi: 3.0.0\n")
	for i := 0; i < openAPINeighbors; i++ {
		co.trees["head"][fmt.Sprintf("api/gen/%04d.json", i)] = "{}"
	}
	m := newMat(t, co)
	if err := m.writeOpenAPI(context.Background(), "new", "api/openapi.yaml"); err != nil {
		t.Fatal(err)
	}
	if m.count["new"] != openAPINeighbors+1 {
		t.Errorf("copied %d files", m.count["new"])
	}
}

// A module gets every .proto file and buf config under its root (buf.lock
// included), nothing else; module roots are checked in order and one that
// cannot be prepared does not stop the others; an old side without .proto
// files is a new module.
func TestCheckProtoModules(t *testing.T) {
	co := newCheckout(t)
	for _, rev := range []string{"base", "head"} {
		co.trees[rev]["b/buf.yaml"] = "version: v2\n"
		co.trees[rev]["b/buf.lock"] = "# lock\n"
		co.trees[rev]["b/x.proto"] = "syntax = \"proto3\";\n"
		co.trees[rev]["b/README.md"] = "doc"
		co.trees[rev]["a/buf.yaml"] = "version: v2\n"
		co.trees[rev]["a/y.proto"] = "syntax = \"proto3\";\n"
		co.trees[rev]["loose/z.proto"] = "syntax = \"proto3\";\n"
	}
	r := &scriptRunner{answers: map[string]core.ExecResult{"buf": {ExitCode: 2, Stderr: "boom"}}}
	res := protoCheck(t, co, []string{"b/x.proto", "a/y.proto"}, r, nil)
	if len(res.Issues) != 2 || res.Issues[0].Path != "a" || res.Issues[1].Path != "b" {
		t.Fatalf("issues %+v", res.Issues)
	}
	seen := r.seen[1] // module b, after a
	for _, f := range []string{"old/b/buf.lock", "new/b/buf.lock", "new/b/x.proto"} {
		if _, ok := seen[f]; !ok {
			t.Errorf("%s not materialized", f)
		}
	}
	for _, f := range []string{"new/b/README.md", "new/loose/z.proto"} {
		if _, ok := seen[f]; ok {
			t.Errorf("%s materialized", f)
		}
	}

	// Module a cannot be prepared; b is still checked.
	fc := &faultCheckout{memCheckout: co, list: map[string][]string{"head": {"a/../evil.proto"}}}
	r = &scriptRunner{answers: map[string]core.ExecResult{"buf": {}}}
	res = protoCheck(t, fc, []string{"b/x.proto", "a/y.proto"}, r, nil)
	if len(res.Issues) != 1 || res.Issues[0].Path != "a" || len(r.reqs) != 1 || !strings.Contains(strings.Join(r.reqs[0].Command, " "), "new/b") {
		t.Errorf("issues %+v reqs %+v", res.Issues, r.reqs)
	}

	// Only a buf config on the old side: nothing to break.
	co = newCheckout(t)
	co.trees["base"]["p/buf.yaml"] = "version: v2\n"
	co.trees["head"]["p/buf.yaml"] = "version: v2\n"
	co.trees["head"]["p/n.proto"] = "syntax = \"proto3\";\n"
	r = &scriptRunner{answers: map[string]core.ExecResult{"buf": {ExitCode: 100, Stdout: bufOut}}}
	if res := protoCheck(t, co, []string{"p/n.proto"}, r, nil); len(r.reqs) != 0 || len(res.Changes)+len(res.Issues) != 0 {
		t.Errorf("new module: %+v", res)
	}
}

// A compile error is reported as such, without the raw tool output.
func TestCheckProtoCompileErrorReason(t *testing.T) {
	co := newCheckout(t)
	bothSides(co, "a.proto", "syntax = \"proto3\";\n")
	out := `{"path":"new/a.proto","start_line":1,"type":"COMPILE","message":"syntax error"}`
	res := protoCheck(t, co, []string{"a.proto"}, &scriptRunner{answers: map[string]core.ExecResult{"buf": {ExitCode: 100, Stdout: out, Stderr: "noise"}}}, nil)
	if len(res.Issues) != 1 || res.Issues[0].Reason != "buf не смог собрать proto-файлы: new/a.proto: syntax error" {
		t.Errorf("issues %+v", res.Issues)
	}
}

func TestSafeRelNUL(t *testing.T) {
	if safeRel("a\x00b.yaml") {
		t.Error("NUL accepted")
	}
}
