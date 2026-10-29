package crossrepo

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

func TestOldSideRanges(t *testing.T) {
	diff := "diff --git a/app/routes.py b/app/routes.py\nindex 1..2 100644\n--- a/app/routes.py\n+++ b/app/routes.py\n" +
		"@@ -12,2 +12,3 @@ def x():\n-a\n+b\n+c\n x\n@@ -40 +41 @@\n-y\n+z\n@@ -50,0 +52,2 @@\n+n\n+m\n" +
		"diff --git a/new.py b/new.py\nnew file mode 100644\n--- /dev/null\n+++ b/new.py\n@@ -0,0 +1,2 @@\n+a\n+b\n" +
		"diff --git a/gone.py b/gone.py\n--- a/gone.py\n+++ /dev/null\n@@ -1,3 +0,0 @@\n-a\n-b\n-c\n"
	got := oldSideRanges(diff)
	want := map[string][]sysmap.LineRange{
		"app/routes.py": {{From: 12, To: 13}, {From: 40, To: 40}, {From: 50, To: 51}},
		"gone.py":       {{From: 1, To: 3}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ranges = %v, want %v", got, want)
	}
}

func TestAPIEndpoints(t *testing.T) {
	api := &compatcheck.APIRun{Specs: []compat.APIChanges{{Spec: "openapi.yaml", Items: []compat.APIChange{
		{Rule: "endpoint-added", Operation: "POST", Endpoint: "/new"},
		{Rule: "response-property-removed", Operation: "GET", Endpoint: "/orders/{id}"},
		{Rule: "request-property-became-required", Operation: "GET", Endpoint: "/orders/{id}"},
		{Rule: "api-path-removed-without-deprecation", Operation: "DELETE", Endpoint: "/orders/{id}"},
		{Rule: "components-changed"},
	}}}}
	got := apiEndpoints(api)
	want := []sysmap.EndpointRef{{Method: "GET", Path: "/orders/{id}"}, {Method: "DELETE", Path: "/orders/{id}"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("endpoints = %+v", got)
	}
	if apiEndpoints(nil) != nil {
		t.Error("nil api")
	}
}

func TestRenderSysmapBlockEscapes(t *testing.T) {
	imp := &sysmap.Impact{Contracts: []sysmap.Touched{{
		Protocol: sysmap.SSE, Event: "x`y", File: "a.py", Line: 1,
		Usages: []sysmap.Usage{{Repo: "group/web", File: "src/`evil` @all\n/merge.ts", Line: 2}},
	}}}
	blk := renderSysmapBlock(imp, "group/api")
	if strings.Contains(blk, "`evil`") || strings.Contains(blk, "\n/merge") {
		t.Errorf("untrusted text not escaped:\n%s", blk)
	}
	if !strings.Contains(blk, "SSE `x'y`") || !strings.Contains(blk, "1 подписка") {
		t.Errorf("block:\n%s", blk)
	}
}
