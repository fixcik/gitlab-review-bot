package reviewtest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// BigGitDiff is a `git diff` of one file with n added lines (plus the
// headers, which are not changes).
func BigGitDiff(path string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%[1]s b/%[1]s\nnew file mode 100644\n--- /dev/null\n+++ b/%[1]s\n@@ -0,0 +1,%[2]d @@\n", path, n)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "+line %d\n", i)
	}
	return b.String()
}

// BigMRDiffs is the same file as GitLab /diffs returns it (hunks only).
func BigMRDiffs(path string, n int) []core.FileDiff {
	d := BigGitDiff(path, n)
	return []core.FileDiff{{NewPath: path, NewFile: true, Diff: d[strings.Index(d, "@@"):]}}
}

// BookingLimiter tracks the reservation of the review run.
type BookingLimiter struct {
	*FakeLimiter
	mu                 sync.Mutex
	Reserved, Released int
	Committed          int
	LastReq            core.ReserveRequest
}

type TrackedReservation struct {
	l    *BookingLimiter
	in   core.Reservation
	done bool
}

func (b *BookingLimiter) Reserve(ctx context.Context, req core.ReserveRequest) (core.Decision, core.Reservation, error) {
	dec, res, err := core.ReserveByChecks(ctx, b.FakeLimiter, req)
	if res == nil {
		return dec, nil, err
	}
	b.mu.Lock()
	b.Reserved++
	b.LastReq = req
	b.mu.Unlock()
	return dec, &TrackedReservation{l: b, in: res}, err
}

func (t *TrackedReservation) Commit(ctx context.Context) (bool, error) {
	ok, err := t.in.Commit(ctx)
	if ok {
		t.l.mu.Lock()
		t.l.Committed++
		t.l.mu.Unlock()
		t.done = true
	}
	return ok, err
}

func (t *TrackedReservation) Release() {
	t.in.Release()
	if !t.done {
		t.l.mu.Lock()
		t.l.Released++
		t.l.mu.Unlock()
		t.done = true
	}
}

type BigHarness struct {
	*Harness
	Fl      *FakeLint
	Bl      *BookingLimiter
	Skips   []string
	Summary func() string
}

func NewBigHarness(t *testing.T, lines int) *BigHarness {
	t.Helper()
	h, fl := LintHarness(t)
	bh := &BigHarness{Harness: h, Fl: fl}
	bh.Bl = &BookingLimiter{FakeLimiter: h.Lim}
	h.Deps.Limiter = bh.Bl
	h.Deps.OnSkip = func(reason string) { bh.Skips = append(bh.Skips, reason) }
	h.Repos.Diff = BigGitDiff("gen/schema.go", lines)
	h.Repos.Files = []string{"gen/schema.go"}
	h.GL.Diffs = BigMRDiffs("gen/schema.go", lines)
	bh.Summary = func() string {
		h.GL.Mu.Lock()
		defer h.GL.Mu.Unlock()
		st, _ := h.St.Get()
		return h.GL.Notes[st.SummaryNoteID]
	}
	return bh
}

// StalePublished reports whether a leftover draft of an interrupted job
// was published.
func StalePublished(h *Harness) bool {
	h.GL.Mu.Lock()
	defer h.GL.Mu.Unlock()
	for _, d := range h.GL.Published {
		if d.Body == "stale draft" {
			return true
		}
	}
	return false
}
