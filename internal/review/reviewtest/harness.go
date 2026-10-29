package reviewtest

import (
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
)

// The fakes' identities: the bot account, the user issuing commands and the
// MR under review (project PID, MR IID); TestNow is the reviewer's clock.
const (
	BotID   int64 = 99
	BotName       = "reviewbot"
	UserID  int64 = 11
	PID     int64 = 42
	IID     int64 = 7
)

var TestNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// Harness is a whole review.Reviewer on top of the fakes. The fakes are
// exported fields: tests script them (Runner.Resps, GL.Discussions, Cfg)
// and read what the reviewer did (GL.Drafts, GL.Notes, Runner.Reqs).
type Harness struct {
	Compat *FakeCompat
	GL     *FakeGitLab
	Repos  *FakeRepos
	Runner *FakeRunner
	St     *FakeStore
	Lim    *FakeLimiter
	Idx    *FakeIndex
	Cfg    *config.Service
	// Deps are what the reviewer is built from, on the first call of R
	// (New fills them with the fakes above). Set collaborators here
	// before that, the same seam production uses; R panics when Deps changed
	// after the reviewer was built. Reset drops the reviewer.
	Deps review.Deps
	// Enqueue, when set before the first R, is passed to the reviewer's
	// SetEnqueue.
	Enqueue func(core.Job)
	r       *review.Reviewer
	// built: Deps of r.
	built review.Deps
}

// R returns the reviewer, built from Deps on the first call.
func (h *Harness) R() *review.Reviewer {
	if h.r == nil {
		h.r, h.built = review.New(h.Deps), h.Deps
		if h.Enqueue != nil {
			h.r.SetEnqueue(h.Enqueue)
		}
		return h.r
	}
	if f := changedDeps(h.built, h.Deps); f != "" {
		panic("harness: Deps." + f + " changed after the reviewer was built (set it before the first R, or Reset)")
	}
	return h.r
}

// Reset drops the reviewer: the next R builds a new one from Deps.
func (h *Harness) Reset() { h.r = nil }

// Env is the reviewer's shared env (review.Internals), for tests that change
// it while the reviewer runs (a limiter between two jobs) or call its
// helpers.
func (h *Harness) Env() *env.Env { return h.R().Internals().Env }

// Internals are the reviewer's feature instances (review.Internals).
func (h *Harness) Internals() review.Internals { return h.R().Internals() }

// changedDeps names the first field of b that is not the same as in a
// (pointers, funcs and interface values by identity).
func changedDeps(a, b review.Deps) string {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := 0; i < va.NumField(); i++ {
		if !sameValue(va.Field(i), vb.Field(i)) {
			return va.Type().Field(i).Name
		}
	}
	return ""
}

func sameValue(x, y reflect.Value) bool {
	if x.Kind() == reflect.Interface {
		if x.IsNil() || y.IsNil() {
			return x.IsNil() == y.IsNil()
		}
		x, y = x.Elem(), y.Elem()
		if x.Type() != y.Type() {
			return false
		}
	}
	switch x.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Chan, reflect.Slice, reflect.UnsafePointer:
		return x.Pointer() == y.Pointer()
	}
	if x.Type().Comparable() {
		return x.Equal(y)
	}
	return true // a non-comparable value (struct with a func): not checked
}

// New returns a harness with the default config (no .reviewbot.yaml
// required, review.verify off) and Deps filled with the fakes.
func New(t *testing.T) *Harness {
	t.Helper()
	cfg := config.Defaults()
	// The fakes have no .reviewbot.yaml unless a test sets one: review every
	// repository, as before the opt-in (review/repoconfig_test.go turns it on).
	cfg.Review.RequireRepoConfig = false
	// The verification of findings (review.verify, on by default) adds a
	// Claude run whenever a critical/major finding is reported; the tests of
	// other features script one run per review, so it is off here and the
	// verify tests (verify/verify_reviewer_test.go) turn it on.
	cfg.Review.Verify.Enabled = false
	h := &Harness{
		GL: NewFakeGitLab(), Repos: NewFakeRepos(), St: NewFakeStore(), Lim: NewFakeLimiter(),
		Idx: &FakeIndex{Lines: map[string]map[int]int{"cache.go": {1: 1, 2: 0, 3: 0, 4: 2, 5: 3}}},
		Cfg: &cfg, Compat: &FakeCompat{},
	}
	h.Runner = &FakeRunner{Lim: h.Lim}
	h.Deps = review.Deps{
		Compat: h.Compat,
		GitLab: h.GL, Repos: h.Repos, Runner: h.Runner, Store: h.St, Limiter: h.Lim, Config: h.Cfg,
		BuildIndex:    func([]core.FileDiff) env.DiffIndex { return h.Idx },
		RepoConfigTTL: -1, // tests change the fake .reviewbot.yaml between runs
		BotUserID:     BotID,
		BotUsername:   BotName,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:           func() time.Time { return TestNow },
	}
	return h
}

func AutoJob() core.Job {
	return core.Job{Kind: core.JobAutoReview, ProjectID: PID, ProjectPath: "group/api", MRIID: IID, HeadSHA: "head111111111111"}
}

func ManualJob(kind core.JobKind) core.Job {
	return core.Job{Kind: kind, ProjectID: PID, ProjectPath: "group/api", MRIID: IID,
		UserID: UserID, Username: "bob", DiscussionID: "cmd-disc", NoteID: 555}
}

func BotNote(id int64, body string, pos *core.Position) core.Note {
	return core.Note{ID: id, AuthorID: BotID, AuthorUsername: BotName, Body: body, Position: pos}
}
