package review

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
)

func TestParseCheckOutput(t *testing.T) {
	ids := []string{"a", "b", "c"}
	got, err := parseCheckOutput(`{"findings":[
		{"id":" a ","status":"FIXED","comment":" done "},
		{"id":"a","status":"still_open","comment":"dup"},
		{"id":"b","status":"maybe","comment":""},
		{"id":"zzz","status":"fixed","comment":"unknown"}]}`, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a"].Status != checkFixed || got["a"].Comment != "done" || got["b"].Status != checkStillOpen {
		t.Fatalf("got %+v", got)
	}
	if _, ok := got["c"]; ok {
		t.Fatal("missing id must stay missing")
	}
	for _, bad := range []string{`{"findings":[]}`, `{"findings":[{"id":"zzz","status":"fixed","comment":""}]}`, `nope`, `{"findings":[],"extra":1}`} {
		if _, err := parseCheckOutput(bad, ids); err == nil {
			t.Fatalf("%s: want error", bad)
		}
	}
}

func TestRenderCheckLeftCollapses(t *testing.T) {
	var left []checkLeft
	for i := 1; i <= 12; i++ {
		left = append(left, checkLeft{f: mrthread.OpenFinding{Path: "a.go", Line: i, Title: fmt.Sprintf("T%d", i)}})
	}
	got := renderCheckLeft(left)
	if strings.Count(got, "\n- **T") != 12 || !strings.Contains(got, "<details>\n<summary>Ещё 2</summary>\n\n- **T11**") ||
		!strings.HasSuffix(got, "</details>") || strings.Index(got, "T10") > strings.Index(got, "<details>") {
		t.Fatalf("%s", got)
	}
	if renderCheckLeft(nil) != "" {
		t.Fatal("empty")
	}
}
