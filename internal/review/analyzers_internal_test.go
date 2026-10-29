package review

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
)

func TestStepGroupDependenciesAndProblemOrder(t *testing.T) {
	for _, parallel := range []bool{true, false} {
		ctx, run := problems.New(problems.Options{}).Start(context.Background(), core.Job{Kind: core.JobAutoReview, ProjectID: 42, ProjectPath: "group/api", MRIID: 7, HeadSHA: "head111111111111"}) // reviewtest.AutoJob()
		g := newStepGroup(ctx, parallel)
		var order []string
		var mu sync.Mutex
		note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
		slow := g.add("slow", func(ctx context.Context) {
			time.Sleep(50 * time.Millisecond)
			jobctx.BotProblem(ctx, problems.LintFailed, "first", "")
			note("slow")
		})
		g.add("fast", func(ctx context.Context) {
			jobctx.BotProblem(ctx, problems.JiraFailed, "second", "")
			note("fast")
		})
		g.add("after", func(ctx context.Context) {
			jobctx.BotProblem(ctx, problems.CoverageMissing, "third", "")
			note("after")
		}, slow)
		g.wait()
		if strings.Index(strings.Join(order, ","), "slow") > strings.Index(strings.Join(order, ","), "after") {
			t.Fatalf("parallel=%v: dependency not respected: %v", parallel, order)
		}
		if parallel && order[0] != "fast" {
			t.Fatalf("independent step waited: %v", order)
		}
		var subjects []string
		for _, p := range run.List() {
			subjects = append(subjects, p.Subject)
		}
		if got := strings.Join(subjects, ","); got != "first,second,third" {
			t.Fatalf("parallel=%v: problems in order %s", parallel, got)
		}
	}
}

func TestStepGroupPanicReraisedAfterAllSteps(t *testing.T) {
	g := newStepGroup(context.Background(), true)
	var other atomic.Bool
	g.add("boom", func(context.Context) { panic(errors.New("boom")) })
	g.add("other", func(context.Context) { time.Sleep(30 * time.Millisecond); other.Store(true) })
	defer func() {
		p := recover()
		if p == nil || !strings.Contains(p.(string), "review step boom: boom") {
			t.Fatalf("panic: %v", p)
		}
		if !other.Load() {
			t.Fatal("wait returned before every step finished")
		}
	}()
	g.wait()
}

func TestStepGroupCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := newStepGroup(ctx, true)
	// Fill the semaphore with blocked steps is not needed: a cancelled
	// context makes waiting steps give up; whatever ran must not hang.
	for i := 0; i < 3*maxParallel; i++ {
		g.add("s", func(ctx context.Context) { <-ctx.Done() })
	}
	done := make(chan struct{})
	go func() { g.wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait hangs on a cancelled context")
	}
}
