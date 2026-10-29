package reactions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Event is why the bot reacts. The names are the keys of
// review.reactions in the service config and the "event" label of
// reviewbot_reactions_total.
type Event string

const (
	// EventPickedUp: a command or question addressed to the bot was accepted
	// (👀, on the note, right when it is enqueued; again when a postponed
	// job resumes).
	EventPickedUp Event = "picked_up"
	// EventDone: the job finished (👀 is replaced by ✅; an empty emoji only
	// removes 👀).
	EventDone Event = "done"
	// EventFailed: the job failed with an error reply (❌).
	EventFailed Event = "failed"
	// EventPostponed: the job waits for a limit (⏳ instead of 👀).
	EventPostponed Event = "postponed"
	// EventDenied: no access or a per-user limit (🚫, next to the text reply).
	EventDenied Event = "denied"
	// EventThanks: a pure "thanks" to the bot or in a bot thread (❤️, no run).
	EventThanks Event = "thanks"
	// EventAck: a pure acknowledgement in a bot remark thread (👍, no run).
	EventAck Event = "ack"
	// EventLearned: a team rule was learned from the note (🧠).
	EventLearned Event = "learned"
	// EventNoted: a reply in a bot thread was read and led to nothing (👍):
	// no rule and no question, so no answer either.
	EventNoted Event = "noted"
	// EventCleanReview: a review without findings and nothing blocking (🎉 on
	// the MR itself; removed again by a later review that is not clean).
	EventCleanReview Event = "clean_review"
)

// Events lists every event in display order.
func Events() []Event {
	return []Event{EventPickedUp, EventDone, EventFailed, EventPostponed, EventDenied, EventThanks, EventAck, EventLearned, EventNoted, EventCleanReview}
}

// Results of an operation (the "result" label of reviewbot_reactions_total).
const (
	ResultAdded     = "added"     // an award was added (or only removed for a remove-only change)
	ResultExists    = "exists"    // the bot's award was already there: nothing to do
	ResultError     = "error"     // GitLab failed (logged)
	ResultForbidden = "forbidden" // GitLab answered 403 (recorded as a problem once per project)
	ResultDisabled  = "disabled"  // the repository turned reactions off (.reviewbot.yaml)
	ResultDropped   = "dropped"   // the worker queue was full
)

// Target is what gets the award: a note of an MR, or the MR itself when
// NoteID is 0.
type Target struct {
	ProjectID int64
	MRIID     int64
	NoteID    int64
}

func (t Target) String() string {
	if t.NoteID == 0 {
		return fmt.Sprintf("%d!%d", t.ProjectID, t.MRIID)
	}
	return fmt.Sprintf("%d!%d#note_%d", t.ProjectID, t.MRIID, t.NoteID)
}

// API is the award emoji part of the GitLab API (implemented by
// *gitlab.Client). noteID 0 means the MR itself.
type API interface {
	ListAwards(ctx context.Context, projectID, mrIID, noteID int64) ([]core.Award, error)
	AddAward(ctx context.Context, projectID, mrIID, noteID int64, name string) (core.Award, error)
	DeleteAward(ctx context.Context, projectID, mrIID, noteID, awardID int64) error
}

// Change is one reaction update: make sure the bot's awards in Add are on
// the target and its awards in Remove are not (Remove names that are also
// in Add are kept). Other people's awards are never touched.
type Change struct {
	Event       Event
	Target      Target
	ProjectPath string // for the problem record on 403
	Add         []string
	Remove      []string
	// Allowed, when set, is asked in the worker right before the change
	// (the repository's reactions.enabled); false skips it.
	Allowed func(ctx context.Context) bool
}

// StepFunc starts a diagnostics step in ctx (the job's /runs timeline) and
// returns its end function; status is "ok", "error" or "info".
type StepFunc func(ctx context.Context, name string) func(status string, fields map[string]any)

// Options configure a Reactor.
type Options struct {
	API   API
	BotID int64
	// BotUsername identifies the bot's awards when GitLab omits the user id.
	BotUsername string
	// Timeout bounds one change (all its GitLab calls). Default 10s.
	Timeout time.Duration
	// Queue is the worker queue size; changes beyond it are dropped.
	// Default 256.
	Queue int
	// Observe counts every change by event and result (metrics). Optional.
	Observe func(event Event, result string)
	// OnForbidden is called the first time GitLab answers 403 for a
	// project (record a problem). ctx carries the job's values. Optional.
	OnForbidden func(ctx context.Context, projectPath string, err error)
	// Step records a diagnostics step per change. Optional.
	Step   StepFunc
	Logger *slog.Logger
}

type item struct {
	ctx  context.Context
	ch   Change
	done chan struct{}
}

// Reactor applies Changes one by one, in submission order, on a background
// goroutine. Ordering matters: 👀 added when a job is enqueued must be on
// the note before the job's final change replaces it. A Reactor is safe for
// concurrent use; a nil *Reactor ignores everything.
type Reactor struct {
	o     Options
	q     chan item
	start sync.Once

	mu        sync.Mutex
	forbidden map[string]bool // projects already reported on 403
}

// New builds a Reactor; its worker starts with the first change.
func New(o Options) *Reactor {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Queue <= 0 {
		o.Queue = 256
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	o.BotUsername = strings.TrimPrefix(o.BotUsername, "@")
	return &Reactor{o: o, q: make(chan item, o.Queue), forbidden: map[string]bool{}}
}

// closed is returned for changes that are not queued.
var closed = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// Submit queues ch and returns a channel closed when it was applied (or
// dropped). It never blocks. ctx provides values only (the job's
// diagnostics run and problems collector); its cancellation is ignored.
func (r *Reactor) Submit(ctx context.Context, ch Change) <-chan struct{} {
	if r == nil || r.o.API == nil || (len(ch.Add) == 0 && len(ch.Remove) == 0) {
		return closed
	}
	r.start.Do(func() { go r.loop() })
	if ctx == nil {
		ctx = context.Background()
	}
	it := item{ctx: context.WithoutCancel(ctx), ch: ch, done: make(chan struct{})}
	select {
	case r.q <- it:
		return it.done
	default:
		r.observe(ch.Event, ResultDropped)
		r.o.Logger.Warn("reactions: queue full, change dropped", "event", ch.Event, "target", ch.Target.String())
		return closed
	}
}

// Wait waits for done or until d has passed (or ctx is done).
func Wait(ctx context.Context, done <-chan struct{}, d time.Duration) bool {
	if done == nil {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (r *Reactor) loop() {
	for it := range r.q {
		r.apply(it)
	}
}

func (r *Reactor) apply(it item) {
	defer close(it.done)
	defer func() {
		if p := recover(); p != nil {
			r.o.Logger.Error("reactions: change panicked", "event", it.ch.Event, "panic", p)
		}
	}()
	ch := it.ch
	var end func(string, map[string]any)
	if r.o.Step != nil {
		end = r.o.Step(it.ctx, stepName(ch))
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.o.Timeout)
	defer cancel()
	if ch.Allowed != nil && !ch.Allowed(ctx) {
		r.finish(it, end, ResultDisabled, nil)
		return
	}
	res, err := r.change(ctx, ch)
	r.finish(it, end, res, err)
}

func (r *Reactor) finish(it item, end func(string, map[string]any), res string, err error) {
	ch := it.ch
	r.observe(ch.Event, res)
	failed := res == ResultError || res == ResultForbidden
	log := r.o.Logger.With("event", ch.Event, "target", ch.Target.String(), "result", res)
	if failed {
		log.Warn("reactions: change failed", "err", err)
	} else {
		log.Debug("reactions: change applied")
	}
	if res == ResultForbidden && r.o.OnForbidden != nil && r.firstForbidden(ch.ProjectPath) {
		r.o.OnForbidden(it.ctx, ch.ProjectPath, err)
	}
	if end == nil {
		return
	}
	f := map[string]any{"result": res}
	status := "ok"
	switch {
	case failed:
		status = "error"
		if err != nil {
			f["error"] = err.Error()
		}
	case res == ResultDisabled || res == ResultDropped:
		status = "info"
	}
	end(status, f)
}

func (r *Reactor) firstForbidden(project string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forbidden[project] {
		return false
	}
	r.forbidden[project] = true
	return true
}

func (r *Reactor) observe(e Event, res string) {
	if r.o.Observe != nil {
		r.o.Observe(e, res)
	}
}

func stepName(ch Change) string {
	var parts []string
	for _, n := range ch.Add {
		parts = append(parts, "+:"+n+":")
	}
	for _, n := range ch.Remove {
		if !slices.Contains(ch.Add, n) {
			parts = append(parts, "-:"+n+":")
		}
	}
	where := "на заметке"
	if ch.Target.NoteID == 0 {
		where = "на MR"
	}
	return fmt.Sprintf("реакция %s (%s) %s", strings.Join(parts, " "), ch.Event, where)
}

// change applies ch: one listing, then deletions and additions of the
// bot's own awards only.
func (r *Reactor) change(ctx context.Context, ch Change) (string, error) {
	t := ch.Target
	awards, err := r.o.API.ListAwards(ctx, t.ProjectID, t.MRIID, t.NoteID)
	if err != nil {
		return classifyErr(err), fmt.Errorf("list awards: %w", err)
	}
	mine := map[string][]core.Award{}
	for _, a := range awards {
		if r.isBot(a) {
			mine[a.Name] = append(mine[a.Name], a)
		}
	}
	changed := false
	for _, name := range ch.Remove {
		if slices.Contains(ch.Add, name) {
			continue
		}
		for _, a := range mine[name] {
			if err := r.o.API.DeleteAward(ctx, t.ProjectID, t.MRIID, t.NoteID, a.ID); err != nil && !errors.Is(err, core.ErrNotFound) {
				return classifyErr(err), fmt.Errorf("delete award %s: %w", name, err)
			}
			changed = true
		}
	}
	for _, name := range ch.Add {
		if len(mine[name]) > 0 {
			continue
		}
		if _, err := r.o.API.AddAward(ctx, t.ProjectID, t.MRIID, t.NoteID, name); err != nil {
			if errors.Is(err, core.ErrForbidden) {
				return ResultForbidden, fmt.Errorf("add award %s: %w", name, err)
			}
			// GitLab answers a duplicate award with a 4xx ("has already been
			// taken"): an award added meanwhile (another bot instance, a
			// retried request) is not an error.
			if r.hasAward(ctx, t, name) {
				continue
			}
			return classifyErr(err), fmt.Errorf("add award %s: %w", name, err)
		}
		mine[name] = append(mine[name], core.Award{Name: name})
		changed = true
	}
	if !changed {
		return ResultExists, nil
	}
	return ResultAdded, nil
}

func (r *Reactor) hasAward(ctx context.Context, t Target, name string) bool {
	awards, err := r.o.API.ListAwards(ctx, t.ProjectID, t.MRIID, t.NoteID)
	if err != nil {
		return false
	}
	for _, a := range awards {
		if a.Name == name && r.isBot(a) {
			return true
		}
	}
	return false
}

func (r *Reactor) isBot(a core.Award) bool {
	if r.o.BotID != 0 && a.UserID != 0 {
		return a.UserID == r.o.BotID
	}
	return r.o.BotUsername != "" && strings.EqualFold(a.Username, r.o.BotUsername)
}

func classifyErr(err error) string {
	if errors.Is(err, core.ErrForbidden) {
		return ResultForbidden
	}
	return ResultError
}
