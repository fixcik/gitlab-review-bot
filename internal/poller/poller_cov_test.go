package poller

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// faultState is a memState whose operations can fail (per call, by scope or
// MR) and which counts cursor reads (safe to read while Run is going).
type faultState struct {
	*memState
	getCursor  func(scope string) error
	setCursor  func(scope string) error
	getMR      func(iid int64) error
	saveMR     func(iid int64) error
	cursorRead atomic.Int64
}

func (f *faultState) GetPollCursor(ctx context.Context, s string) (time.Time, bool, error) {
	f.cursorRead.Add(1)
	if f.getCursor != nil {
		if err := f.getCursor(s); err != nil {
			return time.Time{}, false, err
		}
	}
	return f.memState.GetPollCursor(ctx, s)
}

func (f *faultState) SetPollCursor(ctx context.Context, s string, t time.Time) error {
	if f.setCursor != nil {
		if err := f.setCursor(s); err != nil {
			return err
		}
	}
	return f.memState.SetPollCursor(ctx, s, t)
}

func (f *faultState) GetPollMR(ctx context.Context, p, i int64) (*events.PollMR, error) {
	if f.getMR != nil {
		if err := f.getMR(i); err != nil {
			return nil, err
		}
	}
	return f.memState.GetPollMR(ctx, p, i)
}

func (f *faultState) SavePollMR(ctx context.Context, r events.PollMR) error {
	if f.saveMR != nil {
		if err := f.saveMR(r.IID); err != nil {
			return err
		}
	}
	return f.memState.SavePollMR(ctx, r)
}

func failOn[K comparable](want K, err error) func(K) error {
	return func(got K) error {
		if got == want {
			return err
		}
		return nil
	}
}

// faultHarness is a harness whose state is a faultState.
func faultHarness(t *testing.T, groups ...string) (*harness, *faultState) {
	h := newHarness(t, groups...)
	fs := &faultState{memState: h.st}
	h.p.o.State = fs
	return h, fs
}

func TestNewDefaults(t *testing.T) {
	p := New(Options{})
	if p.now == nil || p.log == nil || p.o.Interval != 30*time.Second {
		t.Fatalf("defaults: now %v, interval %v", p.now != nil, p.o.Interval)
	}
	if d := time.Since(p.now()); d < 0 || d > time.Minute {
		t.Fatalf("default clock is not time.Now: %v", d)
	}
	if p := New(Options{Interval: time.Second}); p.o.Interval != time.Second {
		t.Fatalf("interval overridden: %v", p.o.Interval)
	}
}

// Run keeps ticking every interval until the context ends.
func TestRunTicksEveryInterval(t *testing.T) {
	h := newHarness(t)
	var ticks atomic.Int64
	enough := make(chan struct{})
	h.p.o.Observe = func(time.Duration, int, error) {
		if ticks.Add(1) == 3 {
			close(enough)
		}
	}
	h.p.o.Now = nil
	h.p.now = time.Now
	h.p.o.Interval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.p.Run(ctx); close(done) }()
	select {
	case <-enough:
	case <-time.After(5 * time.Second):
		t.Fatalf("only %d ticks", ticks.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with the context")
	}
}

// A failure caused by the context ending stops the tick at once: the next
// scopes are not polled and no metrics are observed for a cut round.
func TestTickStopsOnCancelledContext(t *testing.T) {
	h, fs := faultHarness(t, "a", "b")
	h.tick() // baseline both scopes
	observed := false
	h.p.o.Observe = func(time.Duration, int, error) { observed = true }
	ctx, cancel := context.WithCancel(context.Background())
	var scopes []string
	fs.getCursor = func(s string) error {
		scopes = append(scopes, s)
		cancel()
		return errors.New("interrupted")
	}
	_, err := h.p.Tick(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if len(scopes) != 1 || observed {
		t.Fatalf("scopes polled %v, observed %v", scopes, observed)
	}
}

// listFailSource fails the incremental listing only (the baseline works).
type listFailSource struct {
	*fakeGitLab
	fail   bool
	failed []string
}

func (s *listFailSource) ListUpdatedMRs(ctx context.Context, g string, since time.Time) ([]gitlab.MRSummary, error) {
	if s.fail && !since.IsZero() {
		s.failed = append(s.failed, g)
		return nil, errors.New("502")
	}
	return s.fakeGitLab.ListUpdatedMRs(ctx, g, since)
}

// A failed listing of one scope is reported (first error) and leaves its
// cursor; the other scopes are still polled.
func TestListErrorKeepsCursorOtherScopesPolled(t *testing.T) {
	h := newHarness(t, "a", "b")
	src := &listFailSource{fakeGitLab: h.gl}
	h.p.o.Source = src
	h.tick()
	before := h.st.cursors["a"]
	src.fail = true
	h.now = h.now.Add(time.Hour)
	var observedErr error
	h.p.o.Observe = func(_ time.Duration, _ int, err error) { observedErr = err }
	n, err := h.p.Tick(context.Background())
	if err == nil || err.Error() != "502" || n != 0 || observedErr != err {
		t.Fatalf("tick: %d %v (observed %v)", n, err, observedErr)
	}
	if !h.st.cursors["a"].Equal(before) || !h.st.cursors["b"].Equal(before) {
		t.Fatalf("cursors moved: %v", h.st.cursors)
	}
	if len(src.failed) != 2 || src.failed[0] != "a" || src.failed[1] != "b" {
		t.Fatalf("listings tried %v, want both scopes", src.failed)
	}
}

// discCancelSource cancels the round while reading the discussions of an MR.
type discCancelSource struct {
	*fakeGitLab
	cancel func()
	calls  int
}

func (s *discCancelSource) ListDiscussions(context.Context, int64, int64) ([]core.Discussion, error) {
	s.calls++
	s.cancel()
	return nil, errors.New("request canceled")
}

// An MR that fails because the context ended aborts the scope: no further
// MRs, no cursor move, the context's error is returned.
func TestMRFailureOnCancelledContextAbortsScope(t *testing.T) {
	h := newHarness(t)
	h.tick()
	h.touch(1, func(m *gitlab.MRSummary) { m.SHA = "sha1" })
	h.touch(2, func(m *gitlab.MRSummary) { m.SHA = "sha1" })
	before := h.st.cursors[""]
	ctx, cancel := context.WithCancel(context.Background())
	src := &discCancelSource{fakeGitLab: h.gl, cancel: cancel}
	h.p.o.Source = src
	h.now = h.now.Add(time.Minute)
	_, err := h.p.Tick(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if src.calls != 1 || len(h.jobs) != 0 || !h.st.cursors[""].Equal(before) {
		t.Fatalf("calls %d, jobs %v, cursor %v (was %v)", src.calls, kinds(h.jobs), h.st.cursors[""], before)
	}
}

// A store failure saving the new cursor is the tick's error; the jobs of
// the round were already enqueued and are counted.
func TestCursorSaveErrorReported(t *testing.T) {
	h, fs := faultHarness(t)
	h.tick()
	before := h.st.cursors[""]
	h.touch(1, func(m *gitlab.MRSummary) {})
	fs.setCursor = failOn("", errors.New("disk full"))
	h.now = h.now.Add(time.Minute)
	n, err := h.p.Tick(context.Background())
	if err == nil || err.Error() != "disk full" || n != 1 || len(h.jobs) != 1 {
		t.Fatalf("tick: %d %v jobs %v", n, err, kinds(h.jobs))
	}
	if !h.st.cursors[""].Equal(before) {
		t.Fatal("cursor written despite the error")
	}
}

// A store failure while recording the baseline aborts it without a cursor,
// so the baseline is redone on the next tick.
func TestBootstrapSaveErrorKeepsNoCursor(t *testing.T) {
	h, fs := faultHarness(t)
	h.mr(1)
	h.mr(2)
	fs.saveMR = failOn(int64(2), errors.New("locked"))
	if _, err := h.p.Tick(context.Background()); err == nil || err.Error() != "locked" {
		t.Fatalf("err %v", err)
	}
	if _, ok := h.st.cursors[""]; ok {
		t.Fatal("cursor recorded after a failed baseline")
	}
	fs.saveMR = nil
	h.tick()
	if _, ok := h.st.cursors[""]; !ok || len(h.st.mrs) != 2 {
		t.Fatalf("baseline not redone: %v %d", h.st.cursors, len(h.st.mrs))
	}
	if len(h.jobs) != 0 {
		t.Fatalf("baseline enqueued %v", kinds(h.jobs))
	}
}

// A store failure reading an MR's record fails that MR only: the others are
// processed, the cursor does not move past it and it is retried.
func TestMRRecordReadErrorRetried(t *testing.T) {
	h, fs := faultHarness(t)
	h.tick()
	h.touch(1, func(m *gitlab.MRSummary) { m.SHA = "sha1" })
	h.now = h.now.Add(10 * time.Second)
	h.touch(2, func(m *gitlab.MRSummary) { m.SHA = "sha1" })
	fs.getMR = failOn(int64(1), errors.New("busy"))
	js := h.tick()
	if len(js) != 1 || js[0].MRIID != 2 {
		t.Fatalf("jobs %+v", js)
	}
	if c := h.st.cursors[""]; c.After(h.gl.mrs[[2]int64{7, 1}].UpdatedAt) {
		t.Fatalf("cursor %v moved past the failed MR", c)
	}
	fs.getMR = nil
	js = h.tick()
	if len(js) != 1 || js[0].MRIID != 1 {
		t.Fatalf("retry jobs %+v", js)
	}
}

// Any positive interval is kept, however small.
func TestNewKeepsTinyInterval(t *testing.T) {
	if p := New(Options{Interval: 1}); p.o.Interval != 1 {
		t.Fatalf("%v", p.o.Interval)
	}
}

// The tick span records the round's outcome: OK, or the error.
func TestTickSpanStatus(t *testing.T) {
	h, fs := faultHarness(t)
	s := &sink{}
	h.p.o.Tracer = tracing.New(tracing.Options{Exporter: s})
	h.tick()
	fs.getCursor = func(string) error { return errors.New("db locked") }
	if _, err := h.p.Tick(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if len(s.spans) != 2 {
		t.Fatalf("%d spans", len(s.spans))
	}
	if ok := s.spans[0]; ok.Status != tracing.StatusOK {
		t.Errorf("ok round: status %v", ok.Status)
	}
	if bad := s.spans[1]; bad.Status != tracing.StatusError || bad.StatusMsg != "db locked" {
		t.Errorf("failed round: status %v %q", bad.Status, bad.StatusMsg)
	}
}

// Several failing scopes: the tick reports (and observes) the first error
// and counts no jobs for them.
func TestTickReportsFirstError(t *testing.T) {
	h, fs := faultHarness(t, "a", "b")
	fs.getCursor = func(s string) error { return errors.New("cursor " + s) }
	var observed error
	h.p.o.Observe = func(_ time.Duration, _ int, err error) { observed = err }
	n, err := h.p.Tick(context.Background())
	if n != 0 || err == nil || err.Error() != "cursor a" || observed != err {
		t.Fatalf("%d %v (observed %v)", n, err, observed)
	}
}

// reversedSource lists the MRs by IID descending.
type reversedSource struct{ *fakeGitLab }

func (s reversedSource) ListUpdatedMRs(ctx context.Context, group string, since time.Time) ([]gitlab.MRSummary, error) {
	out, err := s.fakeGitLab.ListUpdatedMRs(ctx, group, since)
	sort.Slice(out, func(i, j int) bool { return out[i].IID > out[j].IID })
	return out, err
}

// The cursor moves to the latest updated_at of the round whatever the
// listing order; with failures it stays at the earliest failed MR, even
// when a later MR failed too.
func TestCursorLatestUpdateAndEarliestFailure(t *testing.T) {
	h := newHarness(t)
	h.p.o.Source = reversedSource{h.gl}
	h.tick()
	t0 := h.now
	for iid, d := range map[int64]time.Duration{1: 10 * time.Second, 2: 5 * time.Minute, 3: 10 * time.Minute} {
		m := h.mr(iid)
		m.SHA, m.UpdatedAt = "sha1", t0.Add(d)
	}
	h.now = t0.Add(11 * time.Minute)
	if _, err := h.p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := h.st.cursors[""]; !c.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("cursor %v, want the latest update %v", c, t0.Add(10*time.Minute))
	}

	h2 := newHarness(t)
	h2.p.o.Source = sortedSource{h2.gl}
	h2.tick()
	t0 = h2.now
	for iid, d := range map[int64]time.Duration{1: 10 * time.Second, 2: 5 * time.Minute, 3: 10 * time.Minute} {
		m := h2.mr(iid)
		m.SHA, m.UpdatedAt = "sha1", t0.Add(d)
	}
	h2.gl.discErr[[2]int64{7, 1}] = errors.New("500")
	h2.gl.discErr[[2]int64{7, 2}] = errors.New("500")
	h2.now = t0.Add(11 * time.Minute)
	if _, err := h2.p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := h2.st.cursors[""]; !c.Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("cursor %v, want the earliest failure %v", c, t0.Add(10*time.Second))
	}
}

// The baseline cursor is now minus Overlap, or the latest MR update when
// that is later (clock skew), never an older MR's update.
func TestBootstrapCursor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		updates []time.Duration // relative to now, by IID
		want    time.Duration
	}{
		{"old MRs", []time.Duration{-time.Hour, -10 * time.Minute}, -Overlap},
		{"recent MR first", []time.Duration{-30 * time.Second, -10 * time.Minute}, -30 * time.Second},
		{"MR ahead of the clock", []time.Duration{-time.Hour, time.Minute}, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.p.o.Source = sortedSource{h.gl}
			for i, d := range tc.updates {
				h.mr(int64(i + 1)).UpdatedAt = h.now.Add(d)
			}
			if _, err := h.p.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if c := h.st.cursors[""]; !c.Equal(h.now.Add(tc.want)) {
				t.Fatalf("cursor %v, want %v", c.Sub(h.now), tc.want)
			}
		})
	}
}

// countingSource counts discussion reads per MR.
type countingSource struct {
	*fakeGitLab
	reads map[int64]int
}

func (s countingSource) ListDiscussions(ctx context.Context, p, iid int64) ([]core.Discussion, error) {
	s.reads[iid]++
	return s.fakeGitLab.ListDiscussions(ctx, p, iid)
}

// An MR of an excluded project is skipped before anything is read or
// stored; the tick counts only the jobs of the others.
func TestExcludedMRNotReadNorRecorded(t *testing.T) {
	h := newHarness(t)
	src := countingSource{h.gl, map[int64]int{}}
	h.p.o.Source = src
	h.p.o.Router = events.NewRouter(events.Options{BotUsername: "reviewbot", Excluded: func(p string) bool { return p == "team/skip" }})
	h.tick()
	h.mr(1).ProjectPath = "team/skip"
	h.touch(1, func(m *gitlab.MRSummary) {})
	h.touch(2, func(m *gitlab.MRSummary) {})
	h.now = h.now.Add(time.Minute)
	n, err := h.p.Tick(context.Background())
	if err != nil || n != 1 || len(h.jobs) != 1 || h.jobs[0].MRIID != 2 {
		t.Fatalf("%d %v %+v", n, err, h.jobs)
	}
	if src.reads[1] != 0 {
		t.Fatal("discussions of an excluded MR read")
	}
	if _, ok := h.st.mrs[[2]int64{7, 1}]; ok {
		t.Fatal("excluded MR recorded")
	}
}

// MRs failing on the store or on the discussions count no jobs; the tick
// counts the others' and reports no error (they are retried).
func TestFailedMRsCountNoJobs(t *testing.T) {
	h, fs := faultHarness(t)
	h.tick()
	for iid := int64(1); iid <= 3; iid++ {
		h.touch(iid, func(m *gitlab.MRSummary) {})
	}
	fs.getMR = failOn(int64(1), errors.New("busy"))
	h.gl.discErr[[2]int64{7, 2}] = errors.New("500")
	h.now = h.now.Add(time.Minute)
	n, err := h.p.Tick(context.Background())
	if err != nil || n != 1 || len(h.jobs) != 1 || h.jobs[0].MRIID != 3 {
		t.Fatalf("%d %v %+v", n, err, h.jobs)
	}
}

// diff: an open MR with nothing relevant changed is no MR event.
func TestDiffNothingChanged(t *testing.T) {
	prev := &events.PollMR{State: "opened", Draft: true, SHA: "a", Title: "t"}
	if e := diff(prev, gitlab.MRSummary{State: "opened", Draft: true, SHA: "a", Title: "t"}); e.Action != "" {
		t.Fatalf("unchanged: %+v", e)
	}
	if e := diff(prev, gitlab.MRSummary{State: "opened", Draft: true, SHA: "", Title: "t"}); e.Action != "" {
		t.Fatalf("head unknown: %+v", e)
	}
	if e := diff(prev, gitlab.MRSummary{State: "opened", Draft: true, SHA: "a", Title: "t2"}); e.Action != "update" ||
		e.OldTitle != "t" || e.NewTitle != "t2" || e.OldRev != "" || e.BecameReady {
		t.Fatalf("title: %+v", e)
	}
}
