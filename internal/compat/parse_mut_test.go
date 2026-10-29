package compat

import "testing"

func TestParseOasdiffChangelogFallbacks(t *testing.T) {
	out := `[
	 {"id":"api-removed","text":"","level":3,"operation":"DELETE","path":"/x","baseSource":{"file":"/somewhere/else/x.yaml","line":4}},
	 {"id":"security-changed","text":"global security changed","level":3},
	 {"id":"p","text":"path only","level":2,"path":"/y"}
	]`
	items, errs, warns, err := ParseOasdiffChangelog([]byte(out), "api/o.yaml", false)
	if err != nil || len(items) != 3 || len(errs) != 2 || len(warns) != 1 {
		t.Fatalf("items %+v errs %+v warns %+v err %v", items, errs, warns, err)
	}
	if c := errs[0]; c.Path != "api/o.yaml" || c.Line != 0 || c.Message != "DELETE /x: api-removed" {
		t.Errorf("unknown base file keeps the spec path, empty text falls back to the id: %+v", c)
	}
	if c := errs[1]; c.Message != "global security changed" {
		t.Errorf("no operation: no prefix: %q", c.Message)
	}
	if c := warns[0]; c.Message != "/y: path only" {
		t.Errorf("path only: %q", c.Message)
	}
}
