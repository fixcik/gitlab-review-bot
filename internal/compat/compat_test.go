package compat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// memCheckout is an in-memory core.Checkout: trees[rev][path] = content.
type memCheckout struct {
	dir     string
	trees   map[string]map[string]string
	changed []string
}

func (c *memCheckout) Dir() string { return c.dir }
func (c *memCheckout) Diff(context.Context, string, []string) (string, error) {
	return "", nil
}
func (c *memCheckout) ChangedFiles(context.Context, string, []string) ([]string, error) {
	return c.changed, nil
}
func (c *memCheckout) IsAncestor(context.Context, string) (bool, error) { return true, nil }
func (c *memCheckout) ShowFile(_ context.Context, rev, p string) ([]byte, error) {
	if s, ok := c.trees[rev][p]; ok {
		return []byte(s), nil
	}
	return nil, core.ErrNotFound
}
func (c *memCheckout) ListFiles(_ context.Context, rev string) ([]string, error) {
	var out []string
	for p := range c.trees[rev] {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
func (c *memCheckout) Close() error { return nil }

// scriptRunner answers Exec by the program name and records what the
// scratch dir looked like at that moment.
type scriptRunner struct {
	mu      sync.Mutex
	answers map[string]core.ExecResult
	errs    map[string]error
	reqs    []core.ExecRequest
	seen    []map[string]string // files under Dir per call (scratch only)
}

func (s *scriptRunner) Run(context.Context, core.RunRequest) (*core.RunResult, error) {
	return nil, errors.New("no claude in compat tests")
}
func (s *scriptRunner) Exec(_ context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	files := map[string]string{}
	if strings.HasSuffix(req.Dir, ScratchDir) {
		_ = filepath.WalkDir(req.Dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				b, _ := os.ReadFile(p)
				rel, _ := filepath.Rel(req.Dir, p)
				files[filepath.ToSlash(rel)] = string(b)
			}
			return nil
		})
	}
	s.seen = append(s.seen, files)
	if err := s.errs[req.Command[0]]; err != nil {
		return nil, err
	}
	if a, ok := s.answers[req.Command[0]]; ok {
		return &a, nil
	}
	return &core.ExecResult{ExitCode: 127, Stderr: "sh: 1: " + req.Command[0] + ": not found"}, nil
}

func newCheckout(t *testing.T) *memCheckout {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &memCheckout{dir: dir, trees: map[string]map[string]string{"base": {}, "head": {}}}
}

func check(t *testing.T, co *memCheckout, r *scriptRunner, cfg Config) *Result {
	t.Helper()
	c := &Checker{Runner: r}
	res, err := c.Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ---------------------------------------------------------------------------
// Realistic tool outputs (derived from the tools' formatters, see parse.go)
// ---------------------------------------------------------------------------

// oasdiff breaking --format json: a bare array of formatters.Change.
const oasdiffOut = `[
 {"id":"response-property-removed","text":"removed the optional property 'email' from the response with the '200' status","level":3,"operation":"GET","operationId":"getUser","path":"/users/{id}","section":"paths","baseSource":{"file":"old/api/openapi.yaml","line":41,"column":13},"fingerprint":"a1b2c3d4"},
 {"id":"request-parameter-became-required","text":"the 'query' request parameter 'limit' became required","level":3,"operation":"GET","path":"/users","section":"paths","baseSource":{"file":"old/api/openapi.yaml","line":20,"column":11},"revisionSource":{"file":"new/api/openapi.yaml","line":22,"column":11},"fingerprint":"e5f60718"},
 {"id":"response-property-became-optional","text":"the response property 'name' became optional for the status '200'","level":2,"operation":"GET","path":"/users/{id}","section":"paths","comment":"a branch of allOf was compared on its own"},
 {"id":"api-path-added","text":"endpoint added","level":1,"operation":"POST","path":"/users"}
]`

// buf breaking --error-format json: one externalFileAnnotation per line.
const bufOut = `{"path":"new/proto/user/v1/user.proto","start_line":12,"start_column":3,"end_line":12,"end_column":30,"type":"FIELD_SAME_TYPE","message":"Field \"2\" with name \"age\" on message \"User\" changed type from \"int32\" to \"string\"."}
{"path":"old/proto/user/v1/user.proto","start_line":20,"start_column":1,"end_line":24,"end_column":2,"type":"FIELD_NO_DELETE","message":"Previously present field \"3\" with name \"email\" on message \"User\" was deleted."}
`

// graphql-inspector diff without a TTY: every logger line is prefixed.
const gqlOut = `[log]
Detected the following changes (3) between schemas:

[log] ✖  Field email was removed from object type User
[log] ✖  Type for argument id on field Query.user changed from ID! to String!
[log] ⚠  Enum value ADMIN was added to enum Role
[log] ✔  Field nickname was added to object type User
[error] Detected 2 breaking changes
`

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

func TestDiscover(t *testing.T) {
	contents := map[string]string{
		"svc/api.yaml":          "# spec\nopenapi: 3.0.3\ninfo:\n  title: x\n",
		"svc/legacy.json":       `{"info":{"title":"x"},"swagger":"2.0","paths":{}}`,
		"deploy/values.yaml":    "image: x\nopenapi: no\n", // not top-level? it is top-level, but in values: still sniffed
		"deploy/chart.yaml":     "name: chart\nnested:\n  openapi: 3\n",
		"web/queries.graphql":   "query GetUser { user { id } }\n",
		"web/schema.graphql":    "\"\"\"doc\"\"\"\ntype User { id: ID! }\n",
		"contracts/api.v1.yml":  "info: {}\n",
		"x/openapi-public.yaml": "not sniffed: name matches\n",
	}
	changed := []string{
		"svc/api.yaml", "svc/legacy.json", "deploy/chart.yaml", "proto/a/v1/a.proto",
		"web/queries.graphql", "web/schema.graphql", "gql/schema.gql", "api/types.graphqls",
		"contracts/api.v1.yml", "x/openapi-public.yaml", "main.go", ".reviewbot-compat/new/x.proto",
	}
	got := Discover(changed, Config{OpenAPI: []string{"contracts/*.yml"}}, func(p string) []byte {
		if s, ok := contents[p]; ok {
			return []byte(s)
		}
		return nil
	})
	want := map[string]string{
		"svc/api.yaml":          KindOpenAPI, // content-sniffed
		"svc/legacy.json":       KindOpenAPI, // JSON, top-level swagger key
		"proto/a/v1/a.proto":    KindProto,
		"web/schema.graphql":    KindGraphQL,
		"gql/schema.gql":        KindGraphQL,
		"api/types.graphqls":    KindGraphQL,
		"contracts/api.v1.yml":  KindOpenAPI, // extra glob from config
		"x/openapi-public.yaml": KindOpenAPI, // by name
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, s := range got {
		if want[s.Path] != s.Kind {
			t.Errorf("%s: kind %q, want %q", s.Path, s.Kind, want[s.Path])
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Path > got[i].Path {
			t.Errorf("not sorted: %v", got)
		}
	}
}

func TestLooksLikeOpenAPI(t *testing.T) {
	yes := []string{
		"openapi: 3.1.0\n", "---\n# c\nswagger: '2.0'\n", `{"openapi":"3.0.0"}`, "\xef\xbb\xbfopenapi: 3.0.0",
		`{"info":{"openapi":"nested"},"openapi":"3.0.0"}`,
	}
	no := []string{"", "info:\n  openapi: 3\n", `{"info":{"openapi":"3"}}`, "name: openapi\n", "[1,2]", "{bad json"}
	for _, s := range yes {
		if !LooksLikeOpenAPI([]byte(s)) {
			t.Errorf("not detected: %q", s)
		}
	}
	for _, s := range no {
		if LooksLikeOpenAPI([]byte(s)) {
			t.Errorf("false positive: %q", s)
		}
	}
}

func TestMatchAnyBaseName(t *testing.T) {
	if !matchAny([]string{"**/api.yaml"}, "api.yaml") || !matchAny([]string{"**/api.yaml"}, "a/b/api.yaml") {
		t.Error("**/ should match at any depth incl. root")
	}
	if matchAny([]string{"contracts/*.yml"}, "x/contracts/a.yml") {
		t.Error("anchored glob matched elsewhere")
	}
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

func TestParseOasdiff(t *testing.T) {
	errs, warns, err := ParseOasdiff([]byte(oasdiffOut), "api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 2 || len(warns) != 1 {
		t.Fatalf("errs=%+v warns=%+v", errs, warns)
	}
	removed, required := errs[0], errs[1]
	if removed.Rule != "response-property-removed" || removed.Path != "api/openapi.yaml" || removed.Line != 0 ||
		removed.Fingerprint != "a1b2c3d4" || !strings.HasPrefix(removed.Message, "GET /users/{id}: removed the optional property 'email'") {
		t.Errorf("removal (base line must not be used as new line): %+v", removed)
	}
	if required.Line != 22 || required.Path != "api/openapi.yaml" {
		t.Errorf("revision source line: %+v", required)
	}
	if !strings.Contains(warns[0].Message, "(a branch of allOf") {
		t.Errorf("comment not kept: %+v", warns[0])
	}
	for _, in := range []string{"", "[]", "null"} {
		if e, w, err := ParseOasdiff([]byte(in), "a.yaml"); err != nil || len(e)+len(w) != 0 {
			t.Errorf("%q: %v %v %v", in, e, w, err)
		}
	}
	if _, _, err := ParseOasdiff([]byte("Error: failed to load"), "a.yaml"); err == nil {
		t.Error("garbage must be an error")
	}
	// Wrapped shape and string levels are accepted defensively.
	e, _, err := ParseOasdiff([]byte(`{"changes":[{"id":"x","text":"t","level":"error"}],"diff_empty":false}`), "a.yaml")
	if err != nil || len(e) != 1 {
		t.Errorf("wrapped: %v %v", e, err)
	}
}

func TestParseBuf(t *testing.T) {
	changes, compileErr, err := ParseBuf(bufOut)
	if err != nil || compileErr != "" {
		t.Fatal(err, compileErr)
	}
	if len(changes) != 2 {
		t.Fatalf("%+v", changes)
	}
	if c := changes[0]; c.Path != "proto/user/v1/user.proto" || c.Line != 12 || c.Rule != "FIELD_SAME_TYPE" || !strings.Contains(c.Message, `changed type from "int32" to "string"`) {
		t.Errorf("type change: %+v", c)
	}
	if c := changes[1]; c.Path != "proto/user/v1/user.proto" || c.Line != 0 || c.Rule != "FIELD_NO_DELETE" {
		t.Errorf("deletion is located in the old file, no new line: %+v", c)
	}
	_, compileErr, _ = ParseBuf(`{"path":"new/a.proto","start_line":3,"start_column":1,"end_line":3,"end_column":1,"type":"COMPILE","message":"syntax error: unexpected '}'"}`)
	if !strings.Contains(compileErr, "syntax error") {
		t.Errorf("compile error: %q", compileErr)
	}
	if _, _, err := ParseBuf("Failure: something went wrong"); err == nil {
		t.Error("non-JSON output must be an error")
	}
}

func TestParseGraphQLInspector(t *testing.T) {
	br, dang := ParseGraphQLInspector(gqlOut, "schema.graphql")
	if len(br) != 2 || len(dang) != 1 {
		t.Fatalf("breaking=%+v dangerous=%v", br, dang)
	}
	if br[0].Message != "Field email was removed from object type User" || br[0].Path != "schema.graphql" || br[0].Source != SourceGraphQL {
		t.Errorf("%+v", br[0])
	}
	// TTY output with colors and without the [log] prefix.
	br, _ = ParseGraphQLInspector("\x1b[31m✖\x1b[39m  Field \x1b[1memail\x1b[22m was removed\n", "s.graphql")
	if len(br) != 1 || br[0].Message != "Field email was removed" {
		t.Errorf("tty: %+v", br)
	}
}

func TestChangeIDStable(t *testing.T) {
	a := Change{Source: SourceBuf, Path: "a.proto", Line: 10, Rule: "FIELD_NO_DELETE", Message: "x  deleted"}
	b := a
	b.Line = 99
	b.Message = "x deleted"
	if a.ID() != b.ID() {
		t.Error("id must not depend on line or whitespace")
	}
	c := a
	c.Path = "b.proto"
	if a.ID() == c.ID() {
		t.Error("path must matter")
	}
	if len(a.ID()) != 12 {
		t.Error("id length")
	}
}

// ---------------------------------------------------------------------------
// Check (end to end with fakes)
// ---------------------------------------------------------------------------

func TestCheckOpenAPIMaterializesBaseAndHead(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"api/openapi.yaml", "main.go"}
	co.trees["base"]["api/openapi.yaml"] = "openapi: 3.0.0\n# old\n"
	co.trees["head"]["api/openapi.yaml"] = "openapi: 3.0.0\n# new\n"
	co.trees["base"]["api/schemas/user.yaml"] = "type: object # old\n"
	co.trees["head"]["api/schemas/user.yaml"] = "type: object # new\n"
	co.trees["head"]["other/x.yaml"] = "not a sibling\n"
	r := &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {ExitCode: 1, Stdout: oasdiffOut}}}
	res := check(t, co, r, Config{})

	if len(r.reqs) != 1 {
		t.Fatalf("execs: %+v", r.reqs)
	}
	req := r.reqs[0]
	want := "oasdiff changelog --fail-on ERR --format json --lang ru --allow-external-refs=false old/api/openapi.yaml new/api/openapi.yaml"
	if strings.Join(req.Command, " ") != want {
		t.Errorf("argv = %q", req.Command)
	}
	if req.Dir != filepath.Join(co.dir, ScratchDir) {
		t.Errorf("dir = %s", req.Dir)
	}
	seen := r.seen[0]
	if seen["old/api/openapi.yaml"] != "openapi: 3.0.0\n# old\n" || seen["new/api/openapi.yaml"] != "openapi: 3.0.0\n# new\n" {
		t.Errorf("base/head not materialized: %v", seen)
	}
	if seen["old/api/schemas/user.yaml"] == "" || seen["new/api/schemas/user.yaml"] == "" {
		t.Errorf("sibling $ref files missing: %v", seen)
	}
	if _, ok := seen["new/other/x.yaml"]; ok {
		t.Errorf("unrelated file materialized")
	}
	// Scratch dir removed afterwards and excluded from git.
	if _, err := os.Stat(filepath.Join(co.dir, ScratchDir)); !os.IsNotExist(err) {
		t.Errorf("scratch dir not removed: %v", err)
	}
	excl, _ := os.ReadFile(filepath.Join(co.dir, ".git", "info", "exclude"))
	if !strings.Contains(string(excl), "/"+ScratchDir+"/") {
		t.Errorf("not excluded: %q", excl)
	}
	if len(res.Changes) != 2 || len(res.Warnings) != 1 || len(res.Issues) != 0 {
		t.Fatalf("res: %+v", res)
	}
	if !res.SourceOK(SourceOasdiff) {
		t.Error("source should be ok")
	}
}

func TestCheckSkipsNewAndReportsDeletedSpecs(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"new/openapi.yaml", "gone/openapi.yaml"}
	co.trees["head"]["new/openapi.yaml"] = "openapi: 3.0.0\n"
	co.trees["base"]["gone/openapi.yaml"] = "openapi: 3.0.0\n"
	r := &scriptRunner{}
	res := check(t, co, r, Config{})
	if len(r.reqs) != 0 {
		t.Errorf("no tool must run: %+v", r.reqs)
	}
	if len(res.Deleted) != 1 || res.Deleted[0].Path != "gone/openapi.yaml" || len(res.Specs) != 0 {
		t.Errorf("res: %+v", res)
	}
}

func TestCheckMissingToolIsNotBreaking(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"openapi.yaml", "schema.graphql", "a.proto"}
	for _, rev := range []string{"base", "head"} {
		co.trees[rev]["openapi.yaml"] = "openapi: 3.0.0\n"
		co.trees[rev]["schema.graphql"] = "type Q { a: Int }\n"
		co.trees[rev]["a.proto"] = "syntax = \"proto3\";\n"
	}
	res := check(t, co, &scriptRunner{}, Config{})
	if len(res.Changes) != 0 {
		t.Fatalf("missing tools must not produce changes: %+v", res.Changes)
	}
	got := map[string]string{}
	for _, is := range res.Issues {
		got[is.Source] = is.Kind
	}
	for _, s := range []string{SourceOasdiff, SourceBuf, SourceGraphQL} {
		if got[s] != IssueUnavailable {
			t.Errorf("%s: issue %q (%+v)", s, got[s], res.Issues)
		}
		if res.SourceOK(s) {
			t.Errorf("%s must not be ok", s)
		}
	}
}

func TestCheckToolFailureIsAnIssue(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"openapi.yaml"}
	co.trees["base"]["openapi.yaml"] = "openapi: 3.0.0\n"
	co.trees["head"]["openapi.yaml"] = "openapi: 3.0.0\n"
	r := &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {ExitCode: 102, Stderr: "Error: failed to load base spec"}}}
	res := check(t, co, r, Config{})
	if len(res.Changes) != 0 || len(res.Issues) != 1 || res.Issues[0].Kind != IssueFailed ||
		!strings.Contains(res.Issues[0].Reason, "не удалось загрузить спецификацию") {
		t.Fatalf("res: %+v", res)
	}
	// Exec error (timeout).
	r = &scriptRunner{errs: map[string]error{"oasdiff": context.DeadlineExceeded}}
	res = check(t, co, r, Config{})
	if len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Reason, "превышено время") {
		t.Fatalf("timeout: %+v", res.Issues)
	}
}

func TestCheckProtoWithBufYAML(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"proto/user/v1/user.proto"}
	for _, rev := range []string{"base", "head"} {
		co.trees[rev]["proto/buf.yaml"] = "version: v2\n"
		co.trees[rev]["proto/user/v1/user.proto"] = "syntax = \"proto3\"; // " + rev + "\n"
		co.trees[rev]["proto/common/v1/common.proto"] = "syntax = \"proto3\";\n"
		co.trees[rev]["README.md"] = "x"
	}
	r := &scriptRunner{answers: map[string]core.ExecResult{"buf": {ExitCode: 100, Stdout: strings.ReplaceAll(bufOut, "/proto/user", "/proto/user")}}}
	res := check(t, co, r, Config{})
	if len(r.reqs) != 1 {
		t.Fatalf("execs: %+v", r.reqs)
	}
	if got := strings.Join(r.reqs[0].Command, " "); got != "buf breaking new/proto --against old/proto --error-format json" {
		t.Errorf("argv = %q", got)
	}
	seen := r.seen[0]
	for _, f := range []string{"old/proto/buf.yaml", "new/proto/buf.yaml", "old/proto/common/v1/common.proto", "new/proto/user/v1/user.proto"} {
		if _, ok := seen[f]; !ok {
			t.Errorf("%s not materialized: %v", f, seen)
		}
	}
	if _, ok := seen["new/README.md"]; ok {
		t.Error("non-proto file materialized")
	}
	if len(res.Changes) != 2 || res.Changes[0].Source != SourceBuf {
		t.Fatalf("res: %+v", res)
	}
}

func TestCheckProtoWithoutBufYAMLAndCompileError(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"api/a.proto"}
	co.trees["base"]["api/a.proto"] = "syntax = \"proto3\";\n"
	co.trees["head"]["api/a.proto"] = "syntax = \"proto3\"; }\n"
	r := &scriptRunner{answers: map[string]core.ExecResult{"buf": {ExitCode: 100,
		Stderr: `{"path":"new/api/a.proto","start_line":1,"start_column":20,"end_line":1,"end_column":20,"type":"COMPILE","message":"syntax error: unexpected '}'"}`}}}
	res := check(t, co, r, Config{})
	if got := strings.Join(r.reqs[0].Command, " "); got != "buf breaking new --against old --error-format json" {
		t.Errorf("default module argv = %q", got)
	}
	if len(res.Changes) != 0 || len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Reason, "syntax error") {
		t.Fatalf("compile error must be an issue, not a change: %+v", res)
	}
}

func TestCheckGraphQL(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"schema.graphql"}
	co.trees["base"]["schema.graphql"] = "type User { id: ID! email: String }\n"
	co.trees["head"]["schema.graphql"] = "type User { id: ID! }\n"
	r := &scriptRunner{answers: map[string]core.ExecResult{"graphql-inspector": {ExitCode: 1, Stdout: gqlOut}}}
	res := check(t, co, r, Config{})
	if got := strings.Join(r.reqs[0].Command, " "); got != "graphql-inspector diff old/schema.graphql new/schema.graphql" {
		t.Errorf("argv = %q", got)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("res: %+v", res)
	}
	// Exit 1 without any breaking line is a failure (e.g. invalid schema).
	r = &scriptRunner{answers: map[string]core.ExecResult{"graphql-inspector": {ExitCode: 1, Stdout: "[error] Error: Syntax Error: Unexpected Name"}}}
	res = check(t, co, r, Config{})
	if len(res.Changes) != 0 || len(res.Issues) != 1 || res.Issues[0].Kind != IssueFailed {
		t.Fatalf("res: %+v", res)
	}
}

func TestSafeRelAndScratchPath(t *testing.T) {
	for _, p := range []string{"a.yaml", "a/b/c.proto"} {
		if !safeRel(p) {
			t.Errorf("rejected %q", p)
		}
	}
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../../x", "a/./b", `a\b`, "."} {
		if safeRel(p) {
			t.Errorf("accepted %q", p)
		}
	}
	for in, want := range map[string][2]string{
		"old/a/b.yaml":                       {"a/b.yaml", "old"},
		"./new/x.proto":                      {"x.proto", "new"},
		"/work/.reviewbot-compat/new/y.yaml": {"y.yaml", "new"},
		"elsewhere/z":                        {"", ""},
	} {
		p, s := scratchPath(in)
		if p != want[0] || s != want[1] {
			t.Errorf("%s -> %s %s", in, p, s)
		}
	}
}

func TestBufRoots(t *testing.T) {
	roots := bufRoots([]string{"buf.work.yaml", "proto/buf.yaml", "a.proto"})
	if len(roots) != 1 || roots[0] != "." {
		t.Errorf("work at root wins: %v", roots)
	}
	roots = bufRoots([]string{"svc1/proto/buf.yaml", "svc2/buf.yaml", "svc2/inner/buf.yaml"})
	if strings.Join(roots, ",") != "svc2,svc1/proto" {
		t.Errorf("roots: %v", roots)
	}
	if r, ok := rootOf("svc1/proto/a/v1/a.proto", roots); !ok || r != "svc1/proto" {
		t.Errorf("rootOf: %s %v", r, ok)
	}
	if _, ok := rootOf("other/x.proto", roots); ok {
		t.Error("file outside modules")
	}
}

// An MR that commits .reviewbot-compat as a symlink out of the clone must
// not make the bot materialize base/head files there.
func TestScratchDirMaliciousSymlink(t *testing.T) {
	co := newCheckout(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(co.dir, ScratchDir)); err != nil {
		t.Fatal(err)
	}
	co.changed = []string{"api/openapi.yaml"}
	co.trees["base"]["api/openapi.yaml"] = "openapi: 3.0.0\n# old\n"
	co.trees["head"]["api/openapi.yaml"] = "openapi: 3.0.0\n# new\n"
	r := &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {ExitCode: 0, Stdout: "[]"}}}
	check(t, co, r, Config{})
	if len(r.seen) != 1 || r.seen[0]["new/api/openapi.yaml"] == "" {
		t.Fatalf("spec not materialized inside the clone: %v", r.seen)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("wrote outside the clone: %v", ents)
	}
}

// A symlinked directory inside the materialized tree is refused, not followed.
func TestMaterializerRefusesNestedEscape(t *testing.T) {
	co := newCheckout(t)
	outside := t.TempDir()
	m, err := newMaterializer(co, "base", "head")
	if err != nil {
		t.Fatal(err)
	}
	defer m.cleanup()
	if err := os.Symlink(outside, filepath.Join(m.dir, "new", "api")); err != nil {
		t.Fatal(err)
	}
	co.trees["head"]["api/openapi.yaml"] = "x"
	if err := m.write(context.Background(), "new", "api/openapi.yaml"); err == nil {
		t.Fatal("write through an escaping link succeeded")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("wrote outside the clone: %v", ents)
	}
}
