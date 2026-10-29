package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

type innerRunner struct {
	mu    sync.Mutex
	execs int
	runs  []core.RunRequest
}

func (r *innerRunner) Exec(context.Context, core.ExecRequest) (*core.ExecResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execs++
	return &core.ExecResult{ExitCode: 0}, nil
}

func (r *innerRunner) Run(_ context.Context, req core.RunRequest) (*core.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, req)
	return &core.RunResult{Output: `{}`, NumTurns: 1}, nil
}

// The runner chain as serve wires it (instrumentRunner; metrics off):
// claude -> meter -> diag. Every method reaches the inner runner, every
// Claude run is metered with the kind in the context and recorded by diag
// with the same purpose.
func TestRunnerChainForwardsAndAttributes(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "reviewbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec, err := diag.New(diag.Options{Root: filepath.Join(dir, "diag"), Index: st})
	if err != nil {
		t.Fatal(err)
	}
	inner := &innerRunner{}
	cfg := config.Defaults()
	runner := instrumentRunner(instruments{hooks: diag.Hooks{Rec: rec}}, &cfg, inner, st, nil)

	var id string
	job := core.Job{Kind: core.JobReview, ProjectID: 1, ProjectPath: "g/api", MRIID: 5, UserID: 11, Username: "bob"}
	err = rec.Handler(func(ctx context.Context, j core.Job) error {
		id = diag.FromContext(ctx).ID()
		ctx = meter.With(ctx, meter.ForJob(j))
		if _, err := runner.Exec(ctx, core.ExecRequest{Dir: dir, Command: []string{"gitleaks"}}); err != nil {
			return err
		}
		kinds := []string{meter.KindReview, meter.KindVerification, meter.KindMemory, meter.KindLearning}
		for _, k := range kinds {
			meter.Update(ctx, func(a *meter.Attribution) { a.Kind = k })
			if _, err := runner.Run(ctx, core.RunRequest{Dir: dir, Prompt: "p", NoTools: k == meter.KindMemory || k == meter.KindLearning}); err != nil {
				return err
			}
		}
		return nil
	})(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if inner.execs != 1 || len(inner.runs) != 4 {
		t.Fatalf("inner calls: exec=%d runs=%d", inner.execs, len(inner.runs))
	}
	if !inner.runs[2].NoTools || !inner.runs[3].NoTools || inner.runs[0].NoTools {
		t.Fatalf("NoTools not forwarded: %+v", inner.runs)
	}
	groups, err := st.RunsBy(context.Background(), meter.RunFilter{}, meter.ByKind)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, g := range groups {
		got[g.Key] = g.Runs
	}
	for _, k := range []string{meter.KindReview, meter.KindVerification, meter.KindMemory, meter.KindLearning} {
		if got[k] != 1 {
			t.Errorf("metered kinds %v, want one %s", got, k)
		}
	}
	_, r, err := rec.Reader().Get(context.Background(), id)
	if err != nil || r == nil {
		t.Fatalf("diag record: %v", err)
	}
	if len(r.Claude) != 4 || r.Claude[1].Purpose != meter.KindVerification || !r.Claude[2].NoTools {
		t.Fatalf("diag claude runs: %+v", r.Claude)
	}
}
