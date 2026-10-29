package reactions

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

const bot int64 = 99

// fakeAPI keeps awards per target; dupErr makes AddAward fail after adding
// (GitLab's answer to a duplicate seen by a retried request).
type fakeAPI struct {
	mu      sync.Mutex
	awards  map[Target][]core.Award
	next    int64
	adds    []string
	deletes []int64
	addErr  error
	listErr error
	dupErr  bool
	block   chan struct{}
}

func newFakeAPI() *fakeAPI { return &fakeAPI{awards: map[Target][]core.Award{}, next: 100} }

func (f *fakeAPI) ListAwards(_ context.Context, p, m, n int64) ([]core.Award, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]core.Award(nil), f.awards[Target{p, m, n}]...), nil
}

func (f *fakeAPI) AddAward(_ context.Context, p, m, n int64, name string) (core.Award, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds = append(f.adds, name)
	if f.addErr != nil {
		return core.Award{}, f.addErr
	}
	f.next++
	a := core.Award{ID: f.next, Name: name, UserID: bot, Username: "reviewbot"}
	t := Target{p, m, n}
	f.awards[t] = append(f.awards[t], a)
	if f.dupErr {
		return core.Award{}, fmt.Errorf("gitlab: HTTP 404: Award Emoji Name has already been taken: %w", core.ErrNotFound)
	}
	return a, nil
}

func (f *fakeAPI) DeleteAward(_ context.Context, p, m, n, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	t := Target{p, m, n}
	out := f.awards[t][:0]
	for _, a := range f.awards[t] {
		if a.ID != id {
			out = append(out, a)
		}
	}
	f.awards[t] = out
	return nil
}

func (f *fakeAPI) names(t Target) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.awards[t] {
		who := "bot"
		if a.UserID != bot {
			who = a.Username
		}
		out = append(out, who+":"+a.Name)
	}
	sort.Strings(out)
	return out
}

type counts struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *counts) observe(e Event, res string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[string(e)+"/"+res]++
}

func (c *counts) get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

func newTestReactor(api *fakeAPI) (*Reactor, *counts) {
	c := &counts{m: map[string]int{}}
	return New(Options{API: api, BotID: bot, BotUsername: "reviewbot", Observe: c.observe, Timeout: time.Second}), c
}

func wait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	if !Wait(context.Background(), done, 2*time.Second) {
		t.Fatal("change not applied in time")
	}
}

var note = Target{ProjectID: 1, MRIID: 2, NoteID: 3}

func TestReactorLifecycle(t *testing.T) {
	api := newFakeAPI()
	r, c := newTestReactor(api)
	ctx := context.Background()
	// 👀 at enqueue, ⏳ on postpone, 👀 again on resume, ✅ at the end.
	r.Submit(ctx, Change{Event: EventPickedUp, Target: note, Add: []string{"eyes"}})
	r.Submit(ctx, Change{Event: EventPostponed, Target: note, Add: []string{"hourglass_flowing_sand"}, Remove: []string{"eyes"}})
	wait(t, r.Submit(ctx, Change{Event: EventPickedUp, Target: note, Add: []string{"eyes"}, Remove: []string{"hourglass_flowing_sand"}}))
	if got := strings.Join(api.names(note), ","); got != "bot:eyes" {
		t.Fatalf("after resume: %s", got)
	}
	wait(t, r.Submit(ctx, Change{Event: EventDone, Target: note, Add: []string{"white_check_mark"}, Remove: []string{"eyes", "hourglass_flowing_sand"}}))
	if got := strings.Join(api.names(note), ","); got != "bot:white_check_mark" {
		t.Fatalf("after done: %s", got)
	}
	if c.get("picked_up/added") != 2 || c.get("postponed/added") != 1 || c.get("done/added") != 1 {
		t.Errorf("metrics: %v", c.m)
	}
}

func TestReactorIdempotentAndHumanAwards(t *testing.T) {
	api := newFakeAPI()
	api.awards[note] = []core.Award{
		{ID: 1, Name: "eyes", UserID: 5, Username: "alice"},
		{ID: 2, Name: "eyes", UserID: bot, Username: "reviewbot"},
		{ID: 3, Name: "white_check_mark", UserID: 6, Username: "carol"},
	}
	r, c := newTestReactor(api)
	ctx := context.Background()
	// eyes already there: no POST.
	wait(t, r.Submit(ctx, Change{Event: EventPickedUp, Target: note, Add: []string{"eyes"}}))
	if len(api.adds) != 0 || c.get("picked_up/exists") != 1 {
		t.Fatalf("duplicate add: adds=%v metrics=%v", api.adds, c.m)
	}
	// done: the bot's eyes go, alice's stay; carol's ✅ does not count as the bot's.
	wait(t, r.Submit(ctx, Change{Event: EventDone, Target: note, Add: []string{"white_check_mark"}, Remove: []string{"eyes"}}))
	if got := strings.Join(api.names(note), ","); got != "alice:eyes,bot:white_check_mark,carol:white_check_mark" {
		t.Fatalf("awards: %s", got)
	}
	if len(api.deletes) != 1 || api.deletes[0] != 2 {
		t.Errorf("deleted %v, want only the bot's award 2", api.deletes)
	}
	// The same change again: nothing to do.
	wait(t, r.Submit(ctx, Change{Event: EventDone, Target: note, Add: []string{"white_check_mark"}, Remove: []string{"eyes"}}))
	if c.get("done/exists") != 1 || len(api.adds) != 1 {
		t.Errorf("second done: %v adds=%v", c.m, api.adds)
	}
	// A name both added and removed is kept.
	wait(t, r.Submit(ctx, Change{Event: EventDone, Target: note, Add: []string{"white_check_mark"}, Remove: []string{"white_check_mark"}}))
	if got := strings.Join(api.names(note), ","); !strings.Contains(got, "bot:white_check_mark") {
		t.Errorf("kept award removed: %s", got)
	}
}

func TestReactorDuplicateAnswerIsNotAnError(t *testing.T) {
	api := newFakeAPI()
	api.dupErr = true
	r, c := newTestReactor(api)
	wait(t, r.Submit(context.Background(), Change{Event: EventThanks, Target: note, Add: []string{"heart"}}))
	if c.get("thanks/exists") != 1 || c.get("thanks/error") != 0 {
		t.Errorf("metrics: %v", c.m)
	}
	// A real failure (nothing was added) is an error.
	api2 := newFakeAPI()
	api2.addErr = fmt.Errorf("gitlab: HTTP 500")
	r2, c2 := newTestReactor(api2)
	wait(t, r2.Submit(context.Background(), Change{Event: EventThanks, Target: note, Add: []string{"heart"}}))
	if c2.get("thanks/error") != 1 {
		t.Errorf("metrics: %v", c2.m)
	}
}

func TestReactorForbiddenReportedOncePerProject(t *testing.T) {
	api := newFakeAPI()
	api.addErr = fmt.Errorf("gitlab: HTTP 403: %w", core.ErrForbidden)
	var reported []string
	var mu sync.Mutex
	c := &counts{m: map[string]int{}}
	r := New(Options{API: api, BotID: bot, Observe: c.observe, OnForbidden: func(_ context.Context, p string, err error) {
		mu.Lock()
		reported = append(reported, p)
		mu.Unlock()
	}})
	ctx := context.Background()
	r.Submit(ctx, Change{Event: EventPickedUp, Target: note, ProjectPath: "g/a", Add: []string{"eyes"}})
	r.Submit(ctx, Change{Event: EventDone, Target: note, ProjectPath: "g/a", Add: []string{"white_check_mark"}})
	wait(t, r.Submit(ctx, Change{Event: EventDone, Target: note, ProjectPath: "g/b", Add: []string{"white_check_mark"}}))
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(reported, ",") != "g/a,g/b" {
		t.Errorf("reported %v", reported)
	}
	if c.get("picked_up/forbidden") != 1 || c.get("done/forbidden") != 2 {
		t.Errorf("metrics: %v", c.m)
	}
}

func TestReactorAllowedAndStep(t *testing.T) {
	api := newFakeAPI()
	var steps []string
	var mu sync.Mutex
	c := &counts{m: map[string]int{}}
	r := New(Options{API: api, BotID: bot, Observe: c.observe, Step: func(_ context.Context, name string) func(string, map[string]any) {
		return func(status string, f map[string]any) {
			mu.Lock()
			steps = append(steps, fmt.Sprintf("%s [%s %v]", name, status, f["result"]))
			mu.Unlock()
		}
	}})
	ctx := context.Background()
	wait(t, r.Submit(ctx, Change{Event: EventThanks, Target: note, Add: []string{"heart"}, Allowed: func(context.Context) bool { return false }}))
	wait(t, r.Submit(ctx, Change{Event: EventCleanReview, Target: Target{ProjectID: 1, MRIID: 2}, Add: []string{"tada"}}))
	if len(api.adds) != 1 || api.adds[0] != "tada" {
		t.Errorf("adds %v", api.adds)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "реакция +:heart: (thanks) на заметке [info disabled]|реакция +:tada: (clean_review) на MR [ok added]"
	if got := strings.Join(steps, "|"); got != want {
		t.Errorf("steps:\n%s\nwant\n%s", got, want)
	}
}

func TestReactorDropsWhenFullAndNilSafe(t *testing.T) {
	api := newFakeAPI()
	api.block = make(chan struct{})
	c := &counts{m: map[string]int{}}
	r := New(Options{API: api, BotID: bot, Observe: c.observe, Queue: 1})
	ctx := context.Background()
	first := r.Submit(ctx, Change{Event: EventPickedUp, Target: note, Add: []string{"eyes"}})
	// The worker takes the first change and blocks in ListAwards; one more
	// fits in the queue, the next is dropped.
	deadline := time.Now().Add(2 * time.Second)
	for len(r.q) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r.Submit(ctx, Change{Event: EventDone, Target: note, Add: []string{"white_check_mark"}})
	r.Submit(ctx, Change{Event: EventDone, Target: note, Add: []string{"x"}})
	if c.get("done/dropped") != 1 {
		t.Errorf("metrics: %v", c.m)
	}
	close(api.block)
	wait(t, first)

	var nilR *Reactor
	wait(t, nilR.Submit(ctx, Change{Event: EventDone, Target: note, Add: []string{"x"}}))
	// Empty changes are not queued.
	wait(t, r.Submit(ctx, Change{Event: EventDone, Target: note}))
}
