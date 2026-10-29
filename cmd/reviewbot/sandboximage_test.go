package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

// fakeImageDocker: `docker image inspect` fails until a pull succeeds;
// pulls fail while pullErr is set.
type fakeImageDocker struct {
	mu      sync.Mutex
	present bool
	pullErr error
	pulls   int
	pulled  chan struct{}
}

func (f *fakeImageDocker) exec(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k := name + " " + strings.Join(args, " "); !strings.HasPrefix(k, "docker image inspect") {
		return nil, errors.New("unexpected " + k)
	}
	if !f.present {
		return []byte("No such image"), errors.New("exit 1")
	}
	return []byte("sha256:x"), nil
}

func (f *fakeImageDocker) pull(context.Context, string) error {
	f.mu.Lock()
	f.pulls++
	err := f.pullErr
	if err == nil {
		f.present = true
	}
	f.mu.Unlock()
	f.pulled <- struct{}{}
	return err
}

func TestEnsureSandboxImage(t *testing.T) {
	const img = "ghcr.io/fixcik/gitlab-review-bot-sandbox:latest"
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := problems.New(problems.Options{Store: st})
	active := func() []problems.Record {
		rows, err := st.ListProblems(context.Background(), problems.Filter{Status: problems.StatusNew})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	// Present: nothing is pulled.
	f := &fakeImageDocker{present: true, pulled: make(chan struct{}, 4)}
	retry := make(chan time.Time)
	ensureSandboxImage(context.Background(), sandboxImageDeps{image: img, exec: f.exec, pull: f.pull, probs: svc, logger: slog.New(slog.DiscardHandler), retry: func() <-chan time.Time { return retry }})
	if f.pulls != 0 {
		t.Fatalf("present: %d pulls", f.pulls)
	}
	// Missing, the pull fails: a problem; the next try comes only from the
	// retry timer (hourly); it succeeds and resolves the problem.
	f = &fakeImageDocker{pullErr: errors.New("unauthorized: authentication required"), pulled: make(chan struct{}, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		ensureSandboxImage(ctx, sandboxImageDeps{image: img, exec: f.exec, pull: f.pull, probs: svc, logger: slog.New(slog.DiscardHandler), retry: func() <-chan time.Time { return retry }})
		close(done)
	}()
	<-f.pulled
	waitFor(t, func() bool { return len(active()) == 1 })
	if r := active()[0]; r.Kind != problems.SandboxImageMissing || r.Subject != img || !strings.Contains(r.Detail, "unauthorized") || r.ProjectPath != "" {
		t.Fatalf("problem %+v", r)
	}
	f.mu.Lock()
	f.pullErr = nil
	f.mu.Unlock()
	retry <- time.Now()
	<-f.pulled
	<-done
	if f.pulls != 2 || len(active()) != 0 {
		t.Fatalf("pulls %d, active %+v", f.pulls, active())
	}
	// Cancelled while waiting for the retry: returns.
	f = &fakeImageDocker{pullErr: errors.New("no network"), pulled: make(chan struct{}, 4)}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		ensureSandboxImage(ctx2, sandboxImageDeps{image: img, exec: f.exec, pull: f.pull, logger: slog.New(slog.DiscardHandler), retry: func() <-chan time.Time { return retry }})
		close(done2)
	}()
	<-f.pulled
	cancel2()
	<-done2
	// The default retry is an hour; the default pull is the docker CLI.
	d := sandboxImageDeps{}.withDefaults()
	if d.retry == nil || d.pull == nil || d.exec == nil {
		t.Fatal("defaults")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestDockerPullOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := dockerPullOutput(ctx, "img")
	var pe pullError
	if !errors.As(err, &pe) || pe.Error() == "" {
		t.Fatalf("cancelled pull: %v", err)
	}
}
