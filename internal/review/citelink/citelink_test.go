package citelink

import (
	"testing"
)

func TestCitationLinker(t *testing.T) {
	l := New("https://gl.example/")
	l.Add("billing/api", "abcdef1234567")
	l.Add("group/sub/proj", "1234567abcdef")
	l.Add("bad/sha", "not-a-sha")
	base := "https://gl.example/billing/api/-/blob/abcdef1234567/"
	for in, want := range map[string]string{
		"see billing/api:client.go:42":                                 "see [billing/api:client.go:42](" + base + "client.go#L42)",
		"Billing/API:pkg/a.go:1-3, done":                               "[Billing/API:pkg/a.go:1-3](" + base + "pkg/a.go#L1-3), done",
		"file billing/api:docs/README.md.":                             "file [billing/api:docs/README.md](" + base + "docs/README.md).",
		"(group/sub/proj:x/y.go:7)":                                    "([group/sub/proj:x/y.go:7](https://gl.example/group/sub/proj/-/blob/1234567abcdef/x/y.go#L7))",
		"`billing/api:client.go:42`":                                   "[`billing/api:client.go:42`](" + base + "client.go#L42)",
		"`call billing/api:client.go:42 now`":                          "`call billing/api:client.go:42 now`",
		"unknown/repo:x.go:1":                                          "unknown/repo:x.go:1",
		"bad/sha:x.go:1":                                               "bad/sha:x.go:1",
		"https://x.example/billing/api:client.go:1":                    "https://x.example/billing/api:client.go:1",
		"[billing/api:client.go:42](http://other)":                     "[billing/api:client.go:42](http://other)",
		"billing/api:../etc/passwd:1":                                  "billing/api:../etc/passwd:1",
		"billing/api:client.go:0":                                      "billing/api:client.go:0",
		"time 10:30 and a/b:c":                                         "time 10:30 and a/b:c",
		"```\nbilling/api:client.go:42\n```\nbilling/api:client.go:43": "```\nbilling/api:client.go:42\n```\n[billing/api:client.go:43](" + base + "client.go#L43)",
		"~~~~\nbilling/api:a.go:1\n```\nbilling/api:a.go:2\n~~~~":      "~~~~\nbilling/api:a.go:1\n```\nbilling/api:a.go:2\n~~~~",
		"a `unclosed billing/api:a.go:1":                               "a `unclosed [billing/api:a.go:1](" + base + "a.go#L1)",
	} {
		if got := l.Link(in); got != want {
			t.Errorf("link(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	// No GitLab URL: unchanged.
	n := New("")
	n.Add("billing/api", "abcdef1234567")
	if got := n.Link("billing/api:a.go:1"); got != "billing/api:a.go:1" {
		t.Errorf("no base url: %q", got)
	}
}
