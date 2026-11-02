package main

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func TestMergeFindings(t *testing.T) {
	a := outFinding{"a.go", 10, 12, "Nil pointer dereference when the cache is empty", 0, output.Finding{}}
	b := outFinding{"a.go", 11, 11, "Off by one error in the retry loop counter", 0, output.Finding{}}
	repeat := outFinding{"a.go", 10, 13, "Nil pointer dereference when the cache is empty and the handler runs", 1, output.Finding{}}
	other := outFinding{"a.go", 10, 12, "Missing test for the timeout branch of the handler", 1, output.Finding{}}
	far := outFinding{"a.go", 90, 91, "Nil pointer dereference when the cache is empty", 1, output.Finding{}}

	got := mergeFindings([]outFinding{a, b, repeat, other, far})
	// b stays: the same pass never merges with itself. repeat replaces a (same
	// problem, longer text). other stays: overlapping lines, different problem.
	// far stays: a different place.
	if len(got) != 4 {
		t.Fatalf("got %d findings, want 4: %+v", len(got), got)
	}
	if got[0].message != repeat.message {
		t.Errorf("the longer text of a repeated finding should win, got %q", got[0].message)
	}
}
