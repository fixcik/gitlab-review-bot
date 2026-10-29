package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

const drainNote = `{"object_kind":"note","user":{"id":11,"username":"bob"},"project_id":42,
 "project":{"id":42,"path_with_namespace":"g/api"},
 "object_attributes":{"id":555,"note":"@bot help","noteable_type":"MergeRequest","system":false,"discussion_id":"d1"},
 "merge_request":{"iid":7}}`

func postNote(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(drainNote))
	req.Header.Set("X-Gitlab-Event", "Note Hook")
	req.Header.Set("X-Gitlab-Token", "s3cret")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
	}
}

func webhookOnly() *config.Service {
	cfg := config.Defaults()
	cfg.GitLab.Events = config.EventsWebhook
	cfg.GitLab.WebhookSecret = "s3cret"
	return &cfg
}

// A webhook that arrives while the queue drains (SIGTERM, a job still
// running) is persisted, not run and not lost; after the restart the
// restored job runs, and the same event delivered again (the poller of
// events "both" catching up) is dropped as already queued.
func TestWebhookDuringDrainPersistedAndDeduplicatedAfterRestart(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "reviewbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bot := botUser{id: 1, name: "bot", mention: "@bot"}

	q := queue.New(queue.Options{Concurrency: 1, Persist: st, ShutdownGrace: 10 * time.Second, Logger: logger})
	mux := http.NewServeMux()
	done := startEventSources(context.Background(), mux, webhookOnly(), nil, st, bot, q.Enqueue, q.Restore(), nil, nil, logger)
	<-done // nothing polls

	runCtx, stop := context.WithCancel(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	var ran sync.Map
	qDone := make(chan struct{})
	go func() {
		defer close(qDone)
		_ = q.Run(runCtx, func(ctx context.Context, j core.Job) error {
			ran.Store(j.NoteID, true)
			if j.NoteID == 1 {
				close(started)
				<-release
			}
			return nil
		})
	}()
	q.Enqueue(core.Job{Kind: core.JobChat, ProjectID: 42, MRIID: 1, NoteID: 1, DiscussionID: "x"})
	<-started
	stop() // SIGTERM: the drain waits for note 1
	deadline := time.Now().Add(time.Second)
	for !q.Stats().Draining && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	postNote(t, mux) // accepted during the drain
	close(release)
	<-qDone
	if _, ok := ran.Load(int64(555)); ok {
		t.Fatal("the webhook job ran during the drain")
	}
	jobs, err := st.ListJobs(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].Job.NoteID != 555 || jobs[0].Job.Kind != core.JobHelp ||
		jobs[0].Job.DiscussionID != "d1" || jobs[0].Job.Username != "bob" {
		t.Fatalf("persisted after the drain: %+v %v", jobs, err)
	}

	// Restart.
	q2 := queue.New(queue.Options{Concurrency: 1, Persist: st, Logger: logger})
	var accepted atomic.Int32
	mux2 := http.NewServeMux()
	restored := q2.Restore()
	<-startEventSources(context.Background(), mux2, webhookOnly(), nil, st, bot, func(j core.Job) {
		accepted.Add(1)
		q2.Enqueue(j)
	}, restored, nil, nil, logger)
	postNote(t, mux2) // the same event again
	if accepted.Load() != 0 {
		t.Fatal("an event already restored from the queue was enqueued again")
	}
	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	ran2 := make(chan core.Job, 2)
	go func() { _ = q2.Run(ctx2, func(ctx context.Context, j core.Job) error { ran2 <- j; return nil }) }()
	select {
	case j := <-ran2:
		if j.NoteID != 555 {
			t.Fatalf("restored %+v", j)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restored webhook job did not run")
	}
	select {
	case j := <-ran2:
		t.Fatalf("ran twice: %+v", j)
	case <-time.After(50 * time.Millisecond):
	}
}

// A command answered just before a restart is not answered again when the
// same event arrives after it (the poller of events "both" whose record had
// not reached the comment yet): the finished job seeds the dedup.
func TestFinishedJobDeduplicatedAfterRestart(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "reviewbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bot := botUser{id: 1, name: "bot", mention: "@bot"}

	q := queue.New(queue.Options{Concurrency: 1, Persist: st, Logger: logger})
	mux := http.NewServeMux()
	<-startEventSources(context.Background(), mux, webhookOnly(), nil, st, bot, q.Enqueue, q.Restore(), nil, nil, logger)
	ctx, stop := context.WithCancel(context.Background())
	ran := make(chan core.Job, 1)
	qDone := make(chan struct{})
	go func() {
		defer close(qDone)
		_ = q.Run(ctx, func(_ context.Context, j core.Job) error { ran <- j; return nil })
	}()
	postNote(t, mux)
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the command did not run")
	}
	stop()
	<-qDone

	q2 := queue.New(queue.Options{Concurrency: 1, Persist: st, Logger: logger})
	var accepted atomic.Int32
	mux2 := http.NewServeMux()
	<-startEventSources(context.Background(), mux2, webhookOnly(), nil, st, bot, func(core.Job) { accepted.Add(1) }, q2.Restore(), nil, nil, logger)
	postNote(t, mux2)
	if accepted.Load() != 0 {
		t.Fatal("an event answered before the restart was enqueued again")
	}
}

// A listener failure (the port is taken, say) stops the service the way a
// signal does: the queue stops dispatching and its running job — e.g. one
// restored at startup — finishes within the grace before runQueue returns
// the listener's error (serve closes the store only after that).
func TestListenerFailureDrainsTheQueue(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	q := queue.New(queue.Options{Concurrency: 2, ShutdownGrace: 10 * time.Second, Logger: logger})
	started, release := make(chan struct{}), make(chan struct{})
	var finished, second atomic.Bool
	h := func(ctx context.Context, j core.Job) error {
		if j.NoteID == 2 {
			second.Store(true)
			return nil
		}
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		finished.Store(true)
		return nil
	}
	q.Enqueue(core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 1, NoteID: 1})
	listen := make(chan error, 1)
	failure := errors.New("listen tcp :8080: address already in use")
	done := make(chan error, 1)
	go func() { done <- runQueue(context.Background(), q, h, listen, 10*time.Second, logger) }()
	<-started
	listen <- failure
	deadline := time.Now().Add(time.Second)
	for !q.Stats().Draining && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	q.Enqueue(core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 2, NoteID: 2}) // nothing new starts
	select {
	case err := <-done:
		t.Fatalf("returned before the running job finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("err %v, want the listener's", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runQueue did not return after the drain")
	}
	if !finished.Load() || second.Load() {
		t.Fatalf("finished %v, second job ran %v", finished.Load(), second.Load())
	}

	// A signal: the same drain, Run's result (nil).
	q2 := queue.New(queue.Options{Logger: logger})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runQueue(ctx, q2, h, make(chan error), 0, logger); err != nil {
		t.Fatalf("signal: %v", err)
	}
}
