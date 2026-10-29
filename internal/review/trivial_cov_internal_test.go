package review

import (
	"context"
	"testing"
)

// An empty increment is never trivial: the skip needs at least one file
// whose change was looked at (the review flow ends an empty range before,
// this guards the skip decision itself).
func TestClassifyEmptyIncrementNotTrivial(t *testing.T) {
	var r *Reviewer
	inc := r.classifyIncrement(context.Background(), nil, "a", "b", "")
	if inc.trivial || inc.files != 0 || inc.why != "no diff" {
		t.Fatalf("%+v", inc)
	}
}
