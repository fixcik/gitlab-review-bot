package review

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func TestReviewClean(t *testing.T) {
	f := []output.Finding{{Path: "a"}}
	open := &mrthread.Threads{Open: map[string]mrthread.OpenFinding{"x": {}}}
	for i, tc := range []struct {
		ok   bool
		pub  []output.Finding
		out  []output.Finding
		blk  int
		th   *mrthread.Threads
		lr   *linters.Run
		cr   *compatcheck.Run
		part bool
	}{
		{true, nil, nil, 0, &mrthread.Threads{}, nil, nil, false},
		{false, f, nil, 0, nil, nil, nil, false},
		{false, nil, f, 0, nil, nil, nil, false},
		{false, nil, nil, 1, nil, nil, nil, false},
		{false, nil, nil, 0, open, nil, nil, false},
		{false, nil, nil, 0, nil, &linters.Run{Remaining: []linters.Thread{{}}}, nil, false},
		{false, nil, nil, 0, nil, nil, &compatcheck.Run{Outside: []compatcheck.Item{{}}}, false},
		{false, nil, nil, 0, nil, nil, nil, true},
		{true, nil, nil, 0, nil, &linters.Run{}, &compatcheck.Run{}, false},
	} {
		if got := reviewClean(tc.pub, tc.out, tc.blk, tc.th, tc.lr, tc.cr, tc.part); got != tc.ok {
			t.Errorf("case %d: %v", i, got)
		}
	}
}
