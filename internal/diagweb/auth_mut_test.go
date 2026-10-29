package diagweb

import (
	"net/http"
	"strings"
	"testing"
)

func TestParsePathRoutes(t *testing.T) {
	const id = "20261024-120000-abcdef01"
	if !validTestID(id) {
		t.Fatal("fixture id no longer valid: update it to the diag id format")
	}
	for p, want := range map[string]string{
		"/runs":                              "list",
		"/runs/":                             "list",
		"/runs/data.json":                    "listdata",
		"/runs/" + id:                        "run",
		"/runs/" + id + "/":                  "run",
		"/runs/" + id + "/data.json":         "rundata",
		"/runs/" + id + "/claude-1.jsonl":    "transcript",
		"/runs/" + id + "/claude-1000.jsonl": "transcript",
		"/runs/" + id + "/claude-1001.jsonl": "",
		"/runs/" + id + "/claude-0.jsonl":    "",
		"/runs/" + id + "/claude-01.jsonl":   "",
		"/runs/" + id + "/claude-x.jsonl":    "",
		"/runs/" + id + "/x/claude-1.jsonl":  "",
		"/runs/" + id + "/other":             "",
		"/runs/../etc/passwd":                "",
		"/runsx":                             "",
	} {
		route, _, _, ok := parsePath(p)
		if want == "" {
			if ok {
				t.Errorf("%s routed to %q", p, route)
			}
			continue
		}
		if !ok || route != want {
			t.Errorf("%s = %q %v, want %q", p, route, ok, want)
		}
	}
}

// validTestID guards the fixture id above against a change of the id format.
func validTestID(id string) bool {
	route, gotID, _, ok := parsePath("/runs/" + id)
	return ok && route == "run" && gotID == id
}

// With a token, no endpoint leaks run content in its refusal.
func TestRefusalsCarryNoRunData(t *testing.T) {
	f := newFixture(t, Options{Enabled: true, Token: "s3cret"})
	id := f.ids["ok"]
	for _, p := range []string{ListPath, DataPath, "/runs/" + id, "/runs/" + id + "/data.json", "/runs/" + id + "/claude-1.jsonl"} {
		for _, hdr := range []map[string]string{nil, {"Authorization": "Bearer nope"}} {
			rec := get(f.h, p, hdr)
			body := rec.Body.String()
			if rec.Code != http.StatusUnauthorized || strings.Contains(body, "SECRET PROMPT") || strings.Contains(body, "group/api") || strings.Contains(body, "alice") {
				t.Errorf("%s %v: %d %q", p, hdr, rec.Code, body)
			}
		}
	}
}
