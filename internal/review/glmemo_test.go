package review_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// countingGitLab counts the reads that reach the client.
type countingGitLab struct {
	*reviewtest.FakeGitLab
	getMR, diffs, discussions atomic.Int32
	delay                     time.Duration
}

func (c *countingGitLab) GetMR(ctx context.Context, p, i int64) (*core.MergeRequest, error) {
	c.getMR.Add(1)
	time.Sleep(c.delay)
	return c.FakeGitLab.GetMR(ctx, p, i)
}

func (c *countingGitLab) GetMRDiffs(ctx context.Context, p, i int64) ([]core.FileDiff, error) {
	c.diffs.Add(1)
	time.Sleep(c.delay)
	return c.FakeGitLab.GetMRDiffs(ctx, p, i)
}

func (c *countingGitLab) ListDiscussions(ctx context.Context, p, i int64) ([]core.Discussion, error) {
	c.discussions.Add(1)
	time.Sleep(c.delay)
	return c.FakeGitLab.ListDiscussions(ctx, p, i)
}

func TestGitLabMemoReadsOncePerJob(t *testing.T) {
	cg := &countingGitLab{FakeGitLab: reviewtest.NewFakeGitLab()}
	var saved sync.Map
	gl := review.NewMemoGitLab(cg, func(op string) {
		n, _ := saved.LoadOrStore(op, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
	})
	ctx := review.WithGitLabMemo(context.Background())
	for i := 0; i < 3; i++ {
		if _, err := gl.GetMR(ctx, reviewtest.PID, reviewtest.IID); err != nil {
			t.Fatal(err)
		}
		if _, err := gl.GetMRDiffs(ctx, reviewtest.PID, reviewtest.IID); err != nil {
			t.Fatal(err)
		}
		if _, err := gl.ListDiscussions(ctx, reviewtest.PID, reviewtest.IID); err != nil {
			t.Fatal(err)
		}
	}
	if cg.getMR.Load() != 1 || cg.diffs.Load() != 1 || cg.discussions.Load() != 1 {
		t.Fatalf("calls: mr=%d diffs=%d discussions=%d, want 1 each", cg.getMR.Load(), cg.diffs.Load(), cg.discussions.Load())
	}
	for _, op := range []string{review.MemoGetMR, review.MemoGetMRDiffs, review.MemoDiscussions} {
		n, _ := saved.Load(op)
		if n == nil || n.(*atomic.Int32).Load() != 2 {
			t.Errorf("saved %s = %v, want 2", op, n)
		}
	}
	// Another MR is another key.
	if _, err := gl.GetMR(ctx, reviewtest.PID, reviewtest.IID+1); err != nil {
		t.Fatal(err)
	}
	if cg.getMR.Load() != 2 {
		t.Fatalf("other MR not read: %d", cg.getMR.Load())
	}
	// Without a memo in the context (no job) every read is a call.
	gl.GetMR(context.Background(), reviewtest.PID, reviewtest.IID)
	gl.GetMR(context.Background(), reviewtest.PID, reviewtest.IID)
	if cg.getMR.Load() != 4 {
		t.Fatalf("reads outside a job memoized: %d", cg.getMR.Load())
	}
	// A new job starts empty.
	gl.GetMR(review.WithGitLabMemo(context.Background()), reviewtest.PID, reviewtest.IID)
	if cg.getMR.Load() != 5 {
		t.Fatalf("memo leaked between jobs: %d", cg.getMR.Load())
	}
}

func TestGitLabMemoInvalidatedByWrites(t *testing.T) {
	cg := &countingGitLab{FakeGitLab: reviewtest.NewFakeGitLab()}
	cg.PublishMakesThreads = true
	gl := review.NewMemoGitLab(cg, nil)
	ctx := review.WithGitLabMemo(context.Background())
	ds, _ := gl.ListDiscussions(ctx, reviewtest.PID, reviewtest.IID)
	if len(ds) != 0 {
		t.Fatalf("discussions: %v", ds)
	}
	if _, err := gl.CreateDraftNote(ctx, reviewtest.PID, reviewtest.IID, "<!-- reviewbot:finding id=abc -->\nx", nil); err != nil {
		t.Fatal(err)
	}
	if err := gl.PublishDraftNotes(ctx, reviewtest.PID, reviewtest.IID); err != nil {
		t.Fatal(err)
	}
	// The recount after the bot's own publish sees the new thread.
	ds, _ = gl.ListDiscussions(ctx, reviewtest.PID, reviewtest.IID)
	if len(ds) != 1 || cg.discussions.Load() != 2 {
		t.Fatalf("after publish: %d discussions, %d calls", len(ds), cg.discussions.Load())
	}
	writes := []func(){
		func() { gl.CreateNote(ctx, reviewtest.PID, reviewtest.IID, "x") },
		func() { gl.UpdateNote(ctx, reviewtest.PID, reviewtest.IID, 1, "x") },
		func() { gl.ReplyToDiscussion(ctx, reviewtest.PID, reviewtest.IID, "d", "x") },
		func() { gl.ResolveDiscussion(ctx, reviewtest.PID, reviewtest.IID, "d", true) },
		func() { gl.DeleteDraftNotes(ctx, reviewtest.PID, reviewtest.IID) },
		func() {
			review.NewMemoModerator(&fakeMod{}).ResolveDiscussion(ctx, reviewtest.PID, reviewtest.IID, "d", true)
		},
	}
	for i, w := range writes {
		before := cg.discussions.Load()
		gl.ListDiscussions(ctx, reviewtest.PID, reviewtest.IID) // memoized
		if cg.discussions.Load() != before {
			t.Fatalf("write %d: read before the write not memoized", i)
		}
		w()
		gl.ListDiscussions(ctx, reviewtest.PID, reviewtest.IID)
		if cg.discussions.Load() != before+1 {
			t.Fatalf("write %d did not invalidate the memo", i)
		}
	}
	// A write to another MR keeps this MR's memo.
	before := cg.discussions.Load()
	gl.CreateNote(ctx, reviewtest.PID, reviewtest.IID+1, "x")
	gl.ListDiscussions(ctx, reviewtest.PID, reviewtest.IID)
	if cg.discussions.Load() != before {
		t.Fatal("write to another MR invalidated this one")
	}
}

func TestGitLabMemoFreshReadAndCopies(t *testing.T) {
	cg := &countingGitLab{FakeGitLab: reviewtest.NewFakeGitLab()}
	gl := review.NewMemoGitLab(cg, nil)
	ctx := review.WithGitLabMemo(context.Background())
	mr, _ := gl.GetMR(ctx, reviewtest.PID, reviewtest.IID)
	mr.Title = "changed by a caller"
	again, _ := gl.GetMR(ctx, reviewtest.PID, reviewtest.IID)
	if again.Title == "changed by a caller" {
		t.Fatal("callers share the memoized MR")
	}
	cg.Mu.Lock()
	cg.MR.DiffRefs.HeadSHA = "pushed0000000000"
	cg.Mu.Unlock()
	if m, _ := gl.GetMR(ctx, reviewtest.PID, reviewtest.IID); m.DiffRefs.HeadSHA == "pushed0000000000" {
		t.Fatal("memo bypassed without withFreshGitLab")
	}
	fresh, _ := gl.GetMR(jobctx.WithFreshGitLab(ctx), reviewtest.PID, reviewtest.IID)
	if fresh.DiffRefs.HeadSHA != "pushed0000000000" || cg.getMR.Load() != 2 {
		t.Fatalf("fresh read: %s, calls %d", fresh.DiffRefs.HeadSHA, cg.getMR.Load())
	}
	// The fresh result replaced the memo.
	if m, _ := gl.GetMR(ctx, reviewtest.PID, reviewtest.IID); m.DiffRefs.HeadSHA != "pushed0000000000" || cg.getMR.Load() != 2 {
		t.Fatalf("memo not refreshed: %s, calls %d", m.DiffRefs.HeadSHA, cg.getMR.Load())
	}
}

// failingGitLab fails the first GetMR.
type failingGitLab struct {
	*reviewtest.FakeGitLab
	calls atomic.Int32
}

func (f *failingGitLab) GetMR(ctx context.Context, p, i int64) (*core.MergeRequest, error) {
	if f.calls.Add(1) == 1 {
		return nil, context.DeadlineExceeded
	}
	return f.FakeGitLab.GetMR(ctx, p, i)
}

func TestGitLabMemoErrorsNotMemoized(t *testing.T) {
	fg := &failingGitLab{FakeGitLab: reviewtest.NewFakeGitLab()}
	gl := review.NewMemoGitLab(fg, nil)
	ctx := review.WithGitLabMemo(context.Background())
	if _, err := gl.GetMR(ctx, reviewtest.PID, reviewtest.IID); err == nil {
		t.Fatal("want the error")
	}
	if mr, err := gl.GetMR(ctx, reviewtest.PID, reviewtest.IID); err != nil || mr == nil || fg.calls.Load() != 2 {
		t.Fatalf("retry after an error: %v %v calls=%d", mr, err, fg.calls.Load())
	}
	// Optional capabilities stay reachable through the decorator.
	if _, ok := core.GitLabAs[crossrepo.OpenMRs](gl); !ok {
		t.Fatal("OpenMRs hidden by the memo decorator")
	}
}

// panickingGitLab panics in the first GetMR once release is closed.
type panickingGitLab struct {
	*reviewtest.FakeGitLab
	calls            atomic.Int32
	started, release chan struct{}
}

func (p *panickingGitLab) GetMR(ctx context.Context, pid, iid int64) (*core.MergeRequest, error) {
	if p.calls.Add(1) == 1 {
		close(p.started)
		<-p.release
		panic("boom")
	}
	return p.FakeGitLab.GetMR(ctx, pid, iid)
}

// A panic in the shared call must not leave the entry pending forever: the
// panic reaches its own caller, waiters make their own call, and the key is
// not memoized.
func TestGitLabMemoPanicInCallDoesNotHangWaiters(t *testing.T) {
	pg := &panickingGitLab{FakeGitLab: reviewtest.NewFakeGitLab(), started: make(chan struct{}), release: make(chan struct{})}
	gl := review.NewMemoGitLab(pg, nil)
	ctx := review.WithGitLabMemo(context.Background())
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		gl.GetMR(ctx, reviewtest.PID, reviewtest.IID)
	}()
	<-pg.started
	type res struct {
		mr  *core.MergeRequest
		err error
	}
	waiter := make(chan res, 1)
	go func() {
		mr, err := gl.GetMR(ctx, reviewtest.PID, reviewtest.IID)
		waiter <- res{mr, err}
	}()
	time.Sleep(20 * time.Millisecond) // let the waiter block on the pending entry
	close(pg.release)
	if p := <-panicked; p != "boom" {
		t.Fatalf("panic not re-raised to the caller: %v", p)
	}
	select {
	case r := <-waiter:
		if r.err != nil || r.mr == nil {
			t.Fatalf("waiter: %v %v", r.mr, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter hangs after a panic in the shared call")
	}
	if mr, err := gl.GetMR(ctx, reviewtest.PID, reviewtest.IID); err != nil || mr == nil {
		t.Fatalf("read after the panic: %v %v", mr, err)
	}
	if n := pg.calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2 (panicked call + the waiter's own call, then memoized)", n)
	}
}

func TestGitLabMemoConcurrentMissesShareOneCall(t *testing.T) {
	cg := &countingGitLab{FakeGitLab: reviewtest.NewFakeGitLab(), delay: 30 * time.Millisecond}
	gl := review.NewMemoGitLab(cg, nil)
	ctx := review.WithGitLabMemo(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := gl.GetMRDiffs(ctx, reviewtest.PID, reviewtest.IID); err != nil {
				t.Error(err)
			}
			ds, _ := gl.ListDiscussions(ctx, reviewtest.PID, reviewtest.IID)
			for i := range ds {
				ds[i].Resolved = true // a caller's change must not race with others
			}
		}()
	}
	wg.Wait()
	if cg.diffs.Load() != 1 || cg.discussions.Load() != 1 {
		t.Fatalf("calls: diffs=%d discussions=%d, want 1", cg.diffs.Load(), cg.discussions.Load())
	}
}

// fakeMod is a Moderator that records resolved discussions.
type fakeMod struct {
	mu       sync.Mutex
	resolved []string
}

func (m *fakeMod) ResolveDiscussion(_ context.Context, _, _ int64, id string, _ bool) error {
	m.mu.Lock()
	m.resolved = append(m.resolved, id)
	m.mu.Unlock()
	return nil
}

func (*fakeMod) AddReviewer(context.Context, int64, int64, int64) (bool, error) { return false, nil }

func TestGitLabMemoInReview(t *testing.T) {
	for _, memo := range []bool{false, true} {
		h := reviewtest.New(t)
		cg := &countingGitLab{FakeGitLab: h.GL}
		cg.RepoConfig = "language: ru\n"
		cg.PublishMakesThreads = true
		saved := map[string]int{}
		var mu sync.Mutex
		mod := &fakeMod{}
		h.Deps = review.Deps{
			Compat: h.Compat, GitLab: cg, Repos: h.Repos, Runner: h.Runner, Store: h.St, Limiter: h.Lim, Config: h.Cfg,
			Moderator:     mod,
			BuildIndex:    func([]core.FileDiff) env.DiffIndex { return h.Idx },
			RepoConfigTTL: -1, BotUserID: reviewtest.BotID, BotUsername: reviewtest.BotName, Now: func() time.Time { return reviewtest.TestNow },
			OnGitLabSaved: func(op string) { mu.Lock(); saved[op]++; mu.Unlock() },
		}
		h.Reset()
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
			{Path: "cache.go", Line: 2, Severity: "minor", Title: "Nit", Body: "x"},
		}, nil)}}
		ctx := context.Background()
		var err error
		if memo {
			err = h.R().Handle(ctx, reviewtest.AutoJob())
		} else {
			err = h.R().HandleReview(ctx, reviewtest.AutoJob()) // no memo in the context
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("memo=%v: GetMR %d, GetMRDiffs %d, ListDiscussions %d, saved %v", memo, cg.getMR.Load(), cg.diffs.Load(), cg.discussions.Load(), saved)
		// The non-blocking finding was resolved after the publish: the
		// re-read after the bot's own write saw the new thread.
		if len(mod.resolved) != 1 {
			t.Fatalf("memo=%v: non-blocking thread not resolved: %v", memo, mod.resolved)
		}
		if !memo {
			continue
		}
		// GetMR: the start and the fresh re-read of diffIndex.
		if cg.getMR.Load() != 2 || cg.diffs.Load() != 1 || cg.discussions.Load() != 3 {
			t.Fatalf("calls: mr=%d diffs=%d discussions=%d", cg.getMR.Load(), cg.diffs.Load(), cg.discussions.Load())
		}
		if saved[review.MemoDiscussions] != 2 || saved[review.MemoGetMRDiffs] != 1 {
			t.Fatalf("saved: %v", saved)
		}
	}
}
