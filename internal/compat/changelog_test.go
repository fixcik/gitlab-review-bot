package compat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Fixtures are real output of oasdiff 1.34.1 (the version in
// Dockerfile.sandbox):
//
//	changelog-openapi3.json: oasdiff changelog --fail-on ERR --format json
//	  --lang en --allow-external-refs=false old/api/openapi.yaml
//	  new/api/openapi.yaml (testdata/specs/{old,new}/openapi.yaml)
//	changelog-swagger2-converted.json: the same with --lang ru on
//	  testdata/specs/{old,new}/swagger.yaml after oas2to3, i.e. on
//	  {old,new}/api/swagger.yaml.reviewbot-oas3.yaml
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func specFile(t *testing.T, side, name string) string {
	t.Helper()
	return fixture(t, filepath.Join("specs", side, name))
}

func TestParseOasdiffChangelog(t *testing.T) {
	items, errs, warns, err := ParseOasdiffChangelog([]byte(fixture(t, "changelog-openapi3.json")), "api/openapi.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 6 || len(errs) != 3 || len(warns) != 0 {
		t.Fatalf("items=%d errs=%+v warns=%+v", len(items), errs, warns)
	}
	byRule := map[string]APIChange{}
	for _, it := range items {
		byRule[it.Rule] = it
	}
	req := byRule["request-property-became-required"]
	if req.Level != 3 || req.Operation != "POST" || req.Endpoint != "/orders" || req.Line != 14 || req.File != "api/openapi.yaml" ||
		req.Text != "the request property `currency` became required" {
		t.Errorf("required: %+v", req)
	}
	if rm := byRule["api-removed-without-deprecation"]; rm.Level != 3 || rm.Operation != "DELETE" || rm.Line != 0 || rm.File != "api/openapi.yaml" {
		t.Errorf("removal must not carry the old line: %+v", rm)
	}
	if add := byRule["endpoint-added"]; add.Level != 1 || add.Endpoint != "/orders/{id}/history" || add.Line != 35 {
		t.Errorf("added: %+v", add)
	}
	if opt := byRule["new-optional-request-property"]; opt.Level != 1 {
		t.Errorf("info level: %+v", opt)
	}
	// Breaking changes are derived exactly like before (same ids as with
	// `oasdiff breaking`: the fingerprints are identical).
	if errs[0].Rule != "request-property-became-required" || errs[0].Fingerprint != "8d7e807ab9a3" || errs[0].Line != 14 ||
		errs[0].Message != "POST /orders: the request property `currency` became required" {
		t.Errorf("err: %+v", errs[0])
	}
	old, _, _ := ParseOasdiff([]byte(fixture(t, "changelog-openapi3.json")), "api/openapi.yaml")
	for i := range old {
		if old[i].ID() != errs[i].ID() {
			t.Errorf("ParseOasdiff and ParseOasdiffChangelog disagree: %+v vs %+v", old[i], errs[i])
		}
	}
}

func TestParseOasdiffChangelogConvertedDropsLines(t *testing.T) {
	items, errs, _, err := ParseOasdiffChangelog([]byte(fixture(t, "changelog-swagger2-converted.json")), "api/swagger.yaml", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || len(errs) != 2 {
		t.Fatalf("items=%+v errs=%+v", items, errs)
	}
	for _, it := range items {
		if it.Line != 0 || it.File != "api/swagger.yaml" {
			t.Errorf("converted spec: lines of the converted file must be dropped: %+v", it)
		}
	}
	for _, c := range errs {
		if c.Line != 0 || c.Path != "api/swagger.yaml" {
			t.Errorf("converted spec: change must be file-level at the spec: %+v", c)
		}
	}
	if !strings.Contains(items[0].Text, "стало обязательным") {
		t.Errorf("localized text: %q", items[0].Text)
	}
}

func TestSpecVersion(t *testing.T) {
	cases := map[string][2]string{
		"swagger: \"2.0\"\ninfo: {}\n":        {"swagger", "2.0"},
		"# c\nswagger: '2.0' # legacy\n":      {"swagger", "2.0"},
		"openapi: 3.1.0\n":                    {"openapi", "3.1.0"},
		`{"info":{},"swagger":"2.0"}`:         {"swagger", "2.0"},
		`{"openapi":"3.0.3","paths":{}}`:      {"openapi", "3.0.3"},
		`{"swagger":2.0}`:                     {"swagger", "2.0"},
		"info:\n  swagger: 2.0\n":             {"", ""},
		"\"openapi\": \"3.0.0\"\npaths: {}\n": {"openapi", "3.0.0"},
	}
	for in, want := range cases {
		k, v := SpecVersion([]byte(in))
		if k != want[0] || v != want[1] {
			t.Errorf("%q: %q %q, want %q", in, k, v, want)
		}
	}
	if minor("3.0.3") != "3.0" || minor("3.1") != "3.1" || minor("2.0") != "" || minor("") != "" {
		t.Error("minor")
	}
}

func swaggerCheckout(t *testing.T) *memCheckout {
	co := newCheckout(t)
	co.changed = []string{"api/swagger.yaml"}
	co.trees["base"]["api/swagger.yaml"] = specFile(t, "old", "swagger.yaml")
	co.trees["head"]["api/swagger.yaml"] = specFile(t, "new", "swagger.yaml")
	return co
}

func argv(r core.ExecRequest) string { return strings.Join(r.Command, " ") }

func TestCheckSwagger2IsConvertedNotSilentlyEmpty(t *testing.T) {
	co := swaggerCheckout(t)
	r := &scriptRunner{answers: map[string]core.ExecResult{
		"oas2to3": {ExitCode: 0},
		"oasdiff": {ExitCode: 1, Stdout: fixture(t, "changelog-swagger2-converted.json")},
	}}
	res := check(t, co, r, Config{})
	if len(r.reqs) != 3 {
		t.Fatalf("execs: %+v", r.reqs)
	}
	if got := argv(r.reqs[0]); got != "oas2to3 old/api/swagger.yaml old/api/swagger.yaml.reviewbot-oas3.yaml" {
		t.Errorf("convert old: %q", got)
	}
	if got := argv(r.reqs[1]); got != "oas2to3 new/api/swagger.yaml new/api/swagger.yaml.reviewbot-oas3.yaml" {
		t.Errorf("convert new: %q", got)
	}
	if r.reqs[0].Dir != filepath.Join(co.dir, ScratchDir) {
		t.Errorf("converter must run in the scratch dir: %s", r.reqs[0].Dir)
	}
	want := "oasdiff changelog --fail-on ERR --format json --lang ru --allow-external-refs=false old/api/swagger.yaml.reviewbot-oas3.yaml new/api/swagger.yaml.reviewbot-oas3.yaml"
	if got := argv(r.reqs[2]); got != want {
		t.Errorf("oasdiff argv = %q", got)
	}
	if len(res.Changes) != 2 || len(res.Issues) != 0 {
		t.Fatalf("res: %+v", res)
	}
	for _, c := range res.Changes {
		if c.Path != "api/swagger.yaml" || c.Line != 0 {
			t.Errorf("converted spec: %+v", c)
		}
	}
	if len(res.API) != 1 || !res.API[0].Converted || res.API[0].Spec != "api/swagger.yaml" || len(res.API[0].Items) != 3 {
		t.Fatalf("api: %+v", res.API)
	}
}

func TestCheckSwagger2ConverterMissingOrFailing(t *testing.T) {
	co := swaggerCheckout(t)
	// No oas2to3 in the image (exit 127).
	r := &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {ExitCode: 0, Stdout: "[]"}}}
	res := check(t, co, r, Config{})
	if len(r.reqs) != 1 || r.reqs[0].Command[0] != "oas2to3" {
		t.Fatalf("oasdiff must not run on an unconverted Swagger 2.0 spec: %+v", r.reqs)
	}
	if len(res.Issues) != 1 || res.Issues[0].Kind != IssueUnavailable || res.Issues[0].Source != SourceOasdiff ||
		!strings.Contains(res.Issues[0].Reason, "oas2to3 не установлен") || !strings.Contains(res.Issues[0].Reason, "Swagger 2.0") {
		t.Fatalf("issues: %+v", res.Issues)
	}
	if res.SourceOK(SourceOasdiff) || len(res.API) != 0 || len(res.Changes) != 0 {
		t.Errorf("must not look like a clean check: %+v", res)
	}
	// Conversion fails.
	r = &scriptRunner{answers: map[string]core.ExecResult{"oas2to3": {ExitCode: 1, Stderr: "convert: bad parameter"}}}
	res = check(t, co, r, Config{})
	if len(res.Issues) != 1 || res.Issues[0].Kind != IssueFailed || !strings.Contains(res.Issues[0].Reason, "не удалось преобразовать Swagger 2.0") ||
		!strings.Contains(res.Issues[0].Reason, "bad parameter") {
		t.Fatalf("issues: %+v", res.Issues)
	}
}

func TestCheckMigrationConvertsOnlyTheSwaggerSide(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"api/openapi.yaml"}
	co.trees["base"]["api/openapi.yaml"] = specFile(t, "old", "swagger.yaml")
	co.trees["head"]["api/openapi.yaml"] = "openapi: 3.1.0\ninfo: {title: x, version: '2'}\npaths: {}\n"
	removal := `[{"id":"api-path-removed-without-deprecation","text":"api path removed without deprecation","level":3,"operation":"POST","path":"/orders","baseSource":{"file":"old/api/openapi.yaml.reviewbot-oas3.yaml","line":7},"fingerprint":"f1"},
	 {"id":"endpoint-added","text":"endpoint added","level":1,"operation":"GET","path":"/v2/orders","revisionSource":{"file":"new/api/openapi.yaml","line":9},"fingerprint":"f2"}]`
	r := &scriptRunner{answers: map[string]core.ExecResult{"oas2to3": {ExitCode: 0}, "oasdiff": {ExitCode: 1, Stdout: removal}}}
	res := check(t, co, r, Config{})
	if len(r.reqs) != 2 || argv(r.reqs[0]) != "oas2to3 old/api/openapi.yaml old/api/openapi.yaml.reviewbot-oas3.yaml" {
		t.Fatalf("only the old side is converted: %+v", r.reqs)
	}
	// Converted old side is 3.0.3, the new one 3.1.0: --auto-upgrade.
	want := "oasdiff changelog --fail-on ERR --format json --lang ru --allow-external-refs=false --auto-upgrade old/api/openapi.yaml.reviewbot-oas3.yaml new/api/openapi.yaml"
	if got := argv(r.reqs[1]); got != want {
		t.Errorf("argv = %q", got)
	}
	if len(res.Changes) != 1 || res.Changes[0].Path != "api/openapi.yaml" {
		t.Errorf("the converted file name must not leak: %+v", res.Changes)
	}
	if len(res.API) != 1 || res.API[0].Items[1].Line != 9 || res.API[0].Items[0].File != "api/openapi.yaml" {
		t.Errorf("native new side keeps its lines: %+v", res.API)
	}
}

func TestCheckAutoUpgradeOnlyAcrossMinorVersions(t *testing.T) {
	for _, tc := range []struct {
		old, new string
		upgrade  bool
	}{
		{"openapi: 3.0.3\n", "openapi: 3.0.1\n", false},
		{"openapi: 3.0.3\n", "openapi: 3.1.0\n", true},
		{"openapi: \"3.1.0\"\n", "openapi: 3.1.1\n", false},
	} {
		co := newCheckout(t)
		co.changed = []string{"openapi.yaml"}
		co.trees["base"]["openapi.yaml"] = tc.old
		co.trees["head"]["openapi.yaml"] = tc.new
		r := &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {ExitCode: 0, Stdout: "[]"}}}
		check(t, co, r, Config{})
		if got := strings.Contains(argv(r.reqs[0]), "--auto-upgrade"); got != tc.upgrade {
			t.Errorf("%q -> %q: auto-upgrade %v (%s)", tc.old, tc.new, got, argv(r.reqs[0]))
		}
	}
}

func TestCheckLanguage(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"openapi.yaml"}
	co.trees["base"]["openapi.yaml"] = "openapi: 3.0.0\n"
	co.trees["head"]["openapi.yaml"] = "openapi: 3.0.0\n"
	for lang, want := range map[string]string{"": "--lang ru", "ru": "--lang ru", "en": "--lang en", "de": "--lang en"} {
		r := &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {ExitCode: 0, Stdout: "[]"}}}
		res, err := (&Checker{Runner: r}).Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head", Lang: lang})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(argv(r.reqs[0]), want) {
			t.Errorf("%q: %s", lang, argv(r.reqs[0]))
		}
		if len(res.API) != 1 || len(res.API[0].Items) != 0 {
			t.Errorf("a diffed spec without changes is still listed: %+v", res.API)
		}
	}
}

func TestCheckExternalRefs(t *testing.T) {
	spec := func(ref string) string {
		return "openapi: 3.0.0\npaths:\n  /u:\n    get:\n      responses:\n        '200':\n          description: ok\n          content:\n            application/json:\n              schema:\n                $ref: " + ref + "\n"
	}
	run := func(head string, extra map[string]string) (*scriptRunner, *Result) {
		co := newCheckout(t)
		co.changed = []string{"api/openapi.yaml"}
		co.trees["base"]["api/openapi.yaml"] = spec("'#/components/schemas/U'")
		co.trees["head"]["api/openapi.yaml"] = head
		for p, c := range extra {
			co.trees["head"][p] = c
		}
		r := &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {ExitCode: 0, Stdout: "[]"}}}
		return r, check(t, co, r, Config{})
	}
	// Relative file refs (transitively) inside the tree: allowed, the
	// sandbox has no network.
	r, res := run(spec("./schemas/user.yaml"), map[string]string{
		"api/schemas/user.yaml":   "type: object\nproperties:\n  a:\n    $ref: \"common.yaml#/A\"\n",
		"api/schemas/common.yaml": "A: {type: string}\n",
	})
	if len(r.reqs) != 1 || !strings.Contains(argv(r.reqs[0]), "--allow-external-refs=true") || len(res.Issues) != 0 {
		t.Fatalf("relative refs: %+v %+v", r.reqs, res.Issues)
	}
	// A URL, an absolute path or a path out of the tree, also behind a
	// relative file: oasdiff is not run at all.
	for _, tc := range []struct {
		head  string
		extra map[string]string
	}{
		{spec("'http://169.254.169.254/latest/meta-data'"), nil},
		{spec("https://example.com/s.yaml#/U"), nil},
		{spec("//example.com/s.yaml"), nil},
		{spec("/etc/passwd"), nil},
		{spec("../../../../etc/passwd"), nil},
		{spec("file:///etc/passwd"), nil},
		{spec(">-"), nil},
		{spec(`"\u0068ttp://x/y"`), nil},
		{spec("./schemas/user.yaml"), map[string]string{"api/schemas/user.yaml": "properties:\n  a: {$ref: 'http://x/a.yaml'}\n"}},
	} {
		r, res := run(tc.head, tc.extra)
		if len(r.reqs) != 0 || len(res.Issues) != 1 || res.Issues[0].Kind != IssueFailed || res.Issues[0].Source != SourceOasdiff {
			t.Errorf("%q: must be refused before oasdiff: reqs=%+v issues=%+v", tc.head, r.reqs, res.Issues)
		}
	}
	// oasdiff refusing a ref the scan missed is a clear failure.
	co := newCheckout(t)
	co.changed = []string{"openapi.yaml"}
	co.trees["base"]["openapi.yaml"] = "openapi: 3.0.0\n"
	co.trees["head"]["openapi.yaml"] = "openapi: 3.0.0\n"
	r = &scriptRunner{answers: map[string]core.ExecResult{"oasdiff": {ExitCode: 102,
		Stderr: `Error: failed to load base spec from "old/openapi.yaml": encountered disallowed external reference: "x.yaml"`}}}
	res = check(t, co, r, Config{})
	if len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Reason, "загрузка внешних ссылок отключена") {
		t.Errorf("issues: %+v", res.Issues)
	}
}

func TestRefValue(t *testing.T) {
	ok := map[string]string{
		`'#/a'`: "#/a", `"x.yaml#/A"`: "x.yaml#/A", `./a.yaml # c`: "./a.yaml", `"#/b"}`: "#/b", `a.yaml,`: "a.yaml",
	}
	for in, want := range ok {
		if v, good := refValue(in); !good || v != want {
			t.Errorf("%q: %q %v", in, v, good)
		}
	}
	for _, bad := range []string{"", "|", ">-", "&a x", "*a", "!!str x", `"\x68ttp://"`, "'unterminated", "a%3A"} {
		if _, good := refValue(bad); good {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckSummaryOnly(t *testing.T) {
	co := newCheckout(t)
	co.changed = []string{"api/openapi.yaml", "a.proto", "schema.graphql"}
	for _, rev := range []string{"base", "head"} {
		co.trees[rev]["a.proto"] = "syntax = \"proto3\";\n"
		co.trees[rev]["schema.graphql"] = "type Q { a: Int }\n"
	}
	co.trees["base"]["api/openapi.yaml"] = specFile(t, "old", "openapi.yaml")
	co.trees["head"]["api/openapi.yaml"] = specFile(t, "new", "openapi.yaml")
	r := &scriptRunner{answers: map[string]core.ExecResult{
		"oasdiff": {ExitCode: 1, Stdout: fixture(t, "changelog-openapi3.json")},
	}}
	res, err := (&Checker{Runner: r}).Check(context.Background(), Request{
		Checkout: co, Base: "base", Head: "head", SummaryOnly: true, Lang: "en",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.reqs) != 1 || r.reqs[0].Command[0] != "oasdiff" {
		t.Fatalf("summary only: just oasdiff: %+v", r.reqs)
	}
	if len(res.Changes) != 0 || len(res.Warnings) != 0 {
		t.Errorf("summary only must not produce blocking changes: %+v", res.Changes)
	}
	if len(res.Specs) != 1 || res.Specs[0].Kind != KindOpenAPI {
		t.Errorf("specs: %+v", res.Specs)
	}
	if len(res.API) != 1 || len(res.API[0].Items) != 6 {
		t.Errorf("api: %+v", res.API)
	}
}

// ---------------------------------------------------------------------------
// Integration with the real tools (skipped unless oasdiff and oas2to3 are on
// PATH, e.g. in the sandbox image).
// ---------------------------------------------------------------------------

// localRunner runs Exec on the host (no sandbox).
type localRunner struct{ scriptRunner }

func (l *localRunner) Exec(ctx context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	bin, err := exec.LookPath(req.Command[0])
	if err != nil {
		return &core.ExecResult{ExitCode: 127, Stderr: err.Error()}, nil
	}
	cmd := exec.CommandContext(ctx, bin, req.Command[1:]...)
	cmd.Dir = req.Dir
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		return nil, err
	}
	return &core.ExecResult{ExitCode: code, Stdout: out.String(), Stderr: errb.String()}, nil
}

func TestIntegrationRealTools(t *testing.T) {
	for _, tool := range []string{"oasdiff", "oas2to3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	co := newCheckout(t)
	co.changed = []string{"api/openapi.yaml", "legacy/swagger.yaml", "mig/api.yaml"}
	co.trees["base"]["api/openapi.yaml"] = specFile(t, "old", "openapi.yaml")
	co.trees["head"]["api/openapi.yaml"] = specFile(t, "new", "openapi.yaml")
	co.trees["base"]["legacy/swagger.yaml"] = specFile(t, "old", "swagger.yaml")
	co.trees["head"]["legacy/swagger.yaml"] = specFile(t, "new", "swagger.yaml")
	co.trees["base"]["mig/api.yaml"] = specFile(t, "old", "swagger.yaml")
	co.trees["head"]["mig/api.yaml"] = specFile(t, "new", "migrated.yaml")
	res, err := (&Checker{Runner: &localRunner{}}).Check(context.Background(), Request{Checkout: co, Base: "base", Head: "head", Lang: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("issues: %+v", res.Issues)
	}
	rules := map[string]map[string]APIChange{}
	for _, a := range res.API {
		rules[a.Spec] = map[string]APIChange{}
		for _, it := range a.Items {
			rules[a.Spec][it.Rule] = it
		}
	}
	if it := rules["api/openapi.yaml"]["request-property-became-required"]; it.Line != 14 || it.Level != 3 {
		t.Errorf("openapi: %+v", rules["api/openapi.yaml"])
	}
	if _, ok := rules["api/openapi.yaml"]["endpoint-added"]; !ok {
		t.Errorf("openapi: endpoint-added missing")
	}
	sw := rules["legacy/swagger.yaml"]
	if it, ok := sw["request-property-became-required"]; !ok || it.Line != 0 || it.File != "legacy/swagger.yaml" {
		t.Errorf("swagger 2.0 breaking change not found: %+v", sw)
	}
	if _, ok := sw["response-property-type-changed"]; !ok {
		t.Errorf("swagger 2.0 type change not found: %+v", sw)
	}
	mig := rules["mig/api.yaml"]
	if it, ok := mig["request-property-became-required"]; !ok || it.Line == 0 {
		t.Errorf("2.0 -> 3.0 migration: native new side keeps lines: %+v", mig)
	}
	breaking := map[string]int{}
	for _, c := range res.Changes {
		breaking[c.Path]++
		if strings.Contains(c.Path, convertedSuffix) {
			t.Errorf("converted file leaked: %+v", c)
		}
	}
	if breaking["api/openapi.yaml"] != 3 || breaking["legacy/swagger.yaml"] != 2 || breaking["mig/api.yaml"] < 2 {
		t.Errorf("breaking: %v (%+v)", breaking, res.Changes)
	}
}
