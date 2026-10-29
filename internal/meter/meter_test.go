package meter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

type fakeStore struct {
	mu     sync.Mutex
	runs   []ClaudeRun
	prunes []time.Time
}

func (s *fakeStore) RecordRun(_ context.Context, r ClaudeRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, r)
	return nil
}

func (s *fakeStore) PruneRuns(_ context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunes = append(s.prunes, before)
	return 0, nil
}

type fakeRunner struct {
	res   *core.RunResult
	err   error
	execs int
}

func (f *fakeRunner) Exec(context.Context, core.ExecRequest) (*core.ExecResult, error) {
	f.execs++
	return &core.ExecResult{}, nil
}
func (f *fakeRunner) Run(context.Context, core.RunRequest) (*core.RunResult, error) {
	return f.res, f.err
}

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func TestRecordsSuccessWithAttribution(t *testing.T) {
	st := &fakeStore{}
	inner := &fakeRunner{res: &core.RunResult{Output: "SECRET PROMPT ANSWER", CostUSD: 0.4, Duration: 3 * time.Second, NumTurns: 2,
		Usage:  core.TokenUsage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4},
		Models: map[string]core.ModelUsage{"opus": {TokenUsage: core.TokenUsage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}, CostUSD: 0.4}}}}
	m := Wrap(inner, st, func() time.Time { return t0 }, nil)

	ctx := With(context.Background(), Attribution{Kind: KindChat, ProjectID: 42, ProjectPath: "g/api", MRIID: 7, Username: "bob", UserID: 11})
	res, err := m.Run(ctx, core.RunRequest{Prompt: "SECRET PROMPT", Model: "opus"})
	if err != nil || res != inner.res {
		t.Fatalf("run: %v %v", res, err)
	}
	if len(st.runs) != 1 {
		t.Fatalf("runs: %+v", st.runs)
	}
	r := st.runs[0]
	if !r.OK || r.Kind != KindChat || r.ProjectPath != "g/api" || r.MRIID != 7 || r.Username != "bob" || r.UserID != 11 ||
		r.Model != "opus" || r.Usage.Total() != 10 || r.CostUSD != 0.4 || r.Duration != 3*time.Second || r.NumTurns != 2 ||
		!r.At.Equal(t0) || len(r.Models) != 1 || r.ErrorClass != "" {
		t.Fatalf("recorded: %+v", r)
	}
	if len(st.prunes) != 1 || !st.prunes[0].Equal(t0.Add(-DefaultRetention)) {
		t.Fatalf("prunes: %v", st.prunes)
	}
	// Second run the same day does not prune again.
	_, _ = m.Run(ctx, core.RunRequest{})
	if len(st.prunes) != 1 {
		t.Fatalf("pruned twice: %v", st.prunes)
	}
}

func TestRecordsErrors(t *testing.T) {
	st := &fakeStore{}
	inner := &fakeRunner{}
	m := Wrap(inner, st, func() time.Time { return t0 }, nil, Options{Retention: -1})
	ctx := With(context.Background(), Attribution{Kind: KindReview, Username: "alice"})

	inner.err = &core.FailedRun{Err: &core.ErrClaudeRateLimited{Message: "limit"},
		Partial: core.RunResult{CostUSD: 0.3, Usage: core.TokenUsage{Input: 50}, NumTurns: 5}}
	_, err := m.Run(ctx, core.RunRequest{})
	var rl *core.ErrClaudeRateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("error must pass through: %v", err)
	}
	inner.err = fmt.Errorf("claude run aborted: %w", context.DeadlineExceeded)
	_, _ = m.Run(ctx, core.RunRequest{})
	inner.err = errors.New("claude run failed (error_max_turns): x")
	_, _ = m.Run(ctx, core.RunRequest{})

	if len(st.runs) != 3 || len(st.prunes) != 0 {
		t.Fatalf("runs %d prunes %d", len(st.runs), len(st.prunes))
	}
	if r := st.runs[0]; r.OK || r.ErrorClass != "rate_limited" || r.CostUSD != 0.3 || r.Usage.Input != 50 || r.NumTurns != 5 || r.Username != "alice" {
		t.Fatalf("rate limited run: %+v", r)
	}
	if r := st.runs[1]; r.OK || r.ErrorClass != "timeout" || r.Usage.Total() != 0 {
		t.Fatalf("timeout run: %+v", r)
	}
	if st.runs[2].ErrorClass != "max_turns" {
		t.Fatalf("max turns: %+v", st.runs[2])
	}
}

func TestUpdateEnrichesAndUnattributed(t *testing.T) {
	st := &fakeStore{}
	m := Wrap(&fakeRunner{res: &core.RunResult{}}, st, func() time.Time { return t0 }, nil)

	// No attribution at all.
	_, _ = m.Run(context.Background(), core.RunRequest{})
	// Update without With is a no-op.
	Update(context.Background(), func(a *Attribution) { a.Username = "x" })

	ctx := With(context.Background(), ForJob(core.Job{Kind: core.JobAutoReview, ProjectID: 1, ProjectPath: "g/a", MRIID: 2}))
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	Update(child, func(a *Attribution) { a.Username = "author"; a.MRTitle = "feat: x" })
	_, _ = m.Run(ctx, core.RunRequest{}) // parent sees the child's update
	Update(ctx, func(a *Attribution) { a.Kind = KindVerification })
	_, _ = m.Run(child, core.RunRequest{})

	if len(st.runs) != 3 {
		t.Fatalf("runs: %+v", st.runs)
	}
	if r := st.runs[0]; r.Username != "" || r.Kind != KindOther {
		t.Fatalf("unattributed: %+v", r)
	}
	if r := st.runs[1]; r.Username != "author" || r.MRTitle != "feat: x" || r.Kind != KindReview || r.ProjectPath != "g/a" {
		t.Fatalf("enriched: %+v", r)
	}
	if st.runs[2].Kind != KindVerification {
		t.Fatalf("kind update: %+v", st.runs[2])
	}
}

func TestExecPassesThrough(t *testing.T) {
	st := &fakeStore{}
	inner := &fakeRunner{}
	m := Wrap(inner, st, nil, nil)
	if _, err := m.Exec(context.Background(), core.ExecRequest{}); err != nil || inner.execs != 1 || len(st.runs) != 0 {
		t.Fatalf("exec: %v execs=%d runs=%d", err, inner.execs, len(st.runs))
	}
}

func TestForJob(t *testing.T) {
	a := ForJob(core.Job{Kind: core.JobReview, Username: "bob", UserID: 11, ProjectPath: "g/a"})
	if a.Kind != KindReview || a.Username != "bob" || a.UserID != 11 {
		t.Fatalf("manual: %+v", a)
	}
	a = ForJob(core.Job{Kind: core.JobAutoReview, Username: "ignored"})
	if a.Username != "" || a.Kind != KindReview {
		t.Fatalf("auto: %+v", a)
	}
	if KindForJob(core.JobChat) != KindChat || KindForJob(core.JobHelp) != KindOther || KindForJob(core.JobCheck) != KindVerification || KindForJob("learn_reply") != KindLearning {
		t.Fatal("kind mapping")
	}
}

// KindForJob (the run kind on /usage and the metrics label) is taken from
// the kind table; it must be one of the run kinds above and the mapping the
// meter had before the table.
func TestKindForJobEveryKind(t *testing.T) {
	want := map[core.JobKind]string{
		core.JobAutoReview: KindReview, core.JobReview: KindReview, core.JobFullReview: KindReview, core.JobForceReview: KindReview,
		core.JobChat: KindChat, core.JobThreadReply: KindLearning, core.JobRules: KindLearning, core.JobCheck: KindVerification,
	}
	for _, k := range core.AllJobKinds() {
		w, ok := want[k]
		if !ok {
			w = KindOther
		}
		if got := KindForJob(k); got != w {
			t.Errorf("KindForJob(%s) = %q, want %q", k, got, w)
		}
	}
	if KindForJob("unknown") != KindOther {
		t.Error("unknown kind")
	}
}
