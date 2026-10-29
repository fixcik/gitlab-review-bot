package review

// Award emoji ("reactions") on the notes people address to the bot and on
// the MR (internal/reactions, docs/features/reactions.md):
//
//   - 👀 picked_up: on a command or question note when it is enqueued
//     (Accepted, wired around the queue in main) and again when a postponed
//     job resumes; on a thread reply right before the learning run;
//   - when the job ends (Handle): ✅ done / ❌ failed (error) / ⏳ postponed
//     (*core.RetryLater) / 🚫 denied (no access, per-user limit) replace 👀;
//     a thread reply only loses 👀;
//   - 🧠 learned on the note a rule was learned from;
//   - ❤️ thanks / 👍 ack instead of a Claude run (core.JobReact, plain
//     replies in bot threads);
//   - 🎉 clean_review on the MR after a review with nothing to report;
//   - a job the queue drops on restore (answered or expired while the bot
//     was down; Dropped, wired as queue.Options.OnDrop) loses 👀 / ⏳.
//
// Every change goes through one ordered background worker
// (reactions.Reactor): 👀 queued at enqueue time is always applied before
// the job's final change. Handle waits for its last change at most
// reactWait, so the change shows up in the job's /runs timeline without
// holding the job up when GitLab is slow. Failures are logged and counted,
// never returned.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/reactions"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

const (
	// reactWait bounds how long Handle waits for its last reaction.
	reactWait = 3 * time.Second
	// reactRepoTTL: how long a repository's reactions.enabled is cached.
	reactRepoTTL = 2 * time.Minute
	// reactMaxNotes caps the in-memory note state (oldest entries go).
	reactMaxNotes = 5000
)

// lifecycleKind: jobs started by a note addressed to the bot
// (core.KindInfo.Reacts).
func lifecycleKind(k core.JobKind) bool {
	info, _ := k.Info()
	return info.Reacts
}

// reactCfg is review.reactions of the service.
func (r *Reviewer) reactCfg() config.Reactions { return r.env.Cfg.Review.Reactions }

// reactsTo reports whether the job's note may get reactions: reactions on,
// a note, not the bot's own, not an ignored user (other bots).
func (r *Reviewer) reactsTo(job core.Job) bool {
	if r.rx == nil || !r.reactCfg().Enabled || job.NoteID == 0 || job.ProjectID == 0 || job.MRIID == 0 {
		return false
	}
	if (r.env.BotID != 0 && job.UserID == r.env.BotID) || (r.env.BotUsername != "" && strings.EqualFold(job.Username, r.env.BotUsername)) {
		return false
	}
	return !r.reactCfg().Ignored(job.Username)
}

// Accepted returns next wrapped so that every command or question note
// gets 👀 the moment its job is enqueued (before it waits for the MR's
// running job or a worker). Wired in main around the queue's Enqueue.
func (r *Reviewer) Accepted(next func(core.Job)) func(core.Job) {
	return func(job core.Job) {
		r.accepted(job)
		next(job)
	}
}

func (r *Reviewer) accepted(job core.Job) {
	if !lifecycleKind(job.Kind) || !r.reactsTo(job) {
		return
	}
	eyes := r.reactCfg().Emoji(string(reactions.EventPickedUp))
	if eyes == "" {
		return
	}
	r.rx.track(job.NoteID, r.env.Now())
	r.rx.rc.Submit(context.Background(), reactions.Change{
		Event: reactions.EventPickedUp, Target: noteTarget(job), ProjectPath: job.ProjectPath,
		Add: []string{eyes}, Allowed: r.reactAllowedFn(job.ProjectID, job.MRIID),
	})
}

// Dropped removes the bot's 👀 and ⏳ from the note of a job the queue
// dropped on restore instead of running it (result: answered — its reply
// is out but the interrupted attempt never set the final award — or
// expired): nothing will finish that job's reaction lifecycle. Wired in
// main as queue.Options.OnDrop; queued, never blocks.
func (r *Reviewer) Dropped(job core.Job, result string) {
	if !r.reactsTo(job) {
		return
	}
	remove := append(r.emojis(reactions.EventPickedUp), r.emojis(reactions.EventPostponed)...)
	r.rx.forget(job.NoteID)
	r.env.Log.Debug("reactions: job dropped on restore", "mr", job.MRKey(), "note", job.NoteID, "result", result)
	r.reactSubmit(context.Background(), nil, reactions.EventDone, noteTarget(job), nil, remove)
}

// reactBegin puts the job's reaction state into ctx and starts the 👀
// lifecycle of a command or question (reactLifecycle).
func (r *Reviewer) reactBegin(ctx context.Context, job core.Job) (context.Context, *reactState) {
	if r.rx == nil {
		return ctx, nil
	}
	st := &reactState{job: job, marks: &jobctx.Marks{}}
	ctx = context.WithValue(ctx, reactKey{}, st)
	ctx = jobctx.WithMarks(ctx, st.marks)
	r.reactLifecycle(ctx, st)
	return ctx, st
}

// reactLifecycle marks the job's note for the 👀 … ✅ lifecycle. A note
// without 👀 yet (not seen by Accepted) or waiting with ⏳ (a postponed job
// resuming) gets 👀 now.
func (r *Reviewer) reactLifecycle(ctx context.Context, st *reactState) {
	if st == nil || !lifecycleKind(st.job.Kind) || !r.reactsTo(st.job) {
		return
	}
	st.mu.Lock()
	st.note = true
	st.mu.Unlock()
	if seen, postponed := r.rx.state(st.job.NoteID); !seen || postponed {
		r.rx.track(st.job.NoteID, r.env.Now())
		r.reactSubmit(ctx, st, reactions.EventPickedUp, noteTarget(st.job),
			r.emojis(reactions.EventPickedUp), r.emojis(reactions.EventPostponed))
	}
}

// reactEnd applies the job's outcome to its note and waits (briefly) for
// the job's last change.
func (r *Reviewer) reactEnd(ctx context.Context, st *reactState, err error) {
	if r.rx == nil || st == nil {
		return
	}
	st.mu.Lock()
	note, quiet := st.note, st.quiet
	st.mu.Unlock()
	denied, learned := st.marks.Get()
	job := st.job
	if note {
		eyes := r.emojis(reactions.EventPickedUp)
		var rl *core.RetryLater
		switch {
		case errors.As(err, &rl):
			r.rx.setPostponed(job.NoteID)
			r.reactSubmit(ctx, st, reactions.EventPostponed, noteTarget(job), r.emojis(reactions.EventPostponed), eyes)
		case err != nil && (errors.Is(err, context.Canceled) || ctx.Err() != nil):
			// The service is stopping: the queue keeps the job's row and
			// replays it after the restart (unless it already answered or
			// expired), nothing more to say now.
			r.rx.forget(job.NoteID)
		default:
			ev := reactions.EventDone
			switch {
			case err != nil:
				ev = reactions.EventFailed
			case denied:
				ev = reactions.EventDenied
			}
			add := r.emojis(ev)
			if ev == reactions.EventDone && quiet {
				add = nil
			}
			r.rx.forget(job.NoteID)
			r.reactSubmit(ctx, st, ev, noteTarget(job), add, append(eyes, r.emojis(reactions.EventPostponed)...))
		}
	}
	if learned && r.reactsTo(job) {
		r.reactSubmit(ctx, st, reactions.EventLearned, noteTarget(job), r.emojis(reactions.EventLearned), nil)
	} else if st.marks.IsNoted() && err == nil && r.reactsTo(job) {
		r.reactSubmit(ctx, st, reactions.EventNoted, noteTarget(job), r.emojis(reactions.EventNoted), nil)
	}
	st.mu.Lock()
	last := st.last
	st.mu.Unlock()
	if last != nil {
		reactions.Wait(context.WithoutCancel(ctx), last, reactWait)
	}
}

// reactSubmit queues one change for the job (gated by the repository's
// switch) and remembers it as the job's last one.
func (r *Reviewer) reactSubmit(ctx context.Context, st *reactState, ev reactions.Event, t reactions.Target, add, remove []string) {
	if r.rx == nil || (len(add) == 0 && len(remove) == 0) {
		return
	}
	project := ""
	if st != nil {
		project = st.job.ProjectPath
	}
	done := r.rx.rc.Submit(ctx, reactions.Change{
		Event: ev, Target: t, ProjectPath: project, Add: add, Remove: remove,
		Allowed: r.reactAllowedFn(t.ProjectID, t.MRIID),
	})
	if st != nil {
		st.mu.Lock()
		st.last = done
		st.mu.Unlock()
	}
}

// emojis is the configured name of ev as a list (nil when off).
func (r *Reviewer) emojis(ev reactions.Event) []string {
	if n := r.reactCfg().Emoji(string(ev)); n != "" {
		return []string{n}
	}
	return nil
}

func noteTarget(job core.Job) reactions.Target {
	return reactions.Target{ProjectID: job.ProjectID, MRIID: job.MRIID, NoteID: job.NoteID}
}

// reactPickedUp starts the 👀 lifecycle for a job that is not a command
// (a thread reply right before its learning run); its "done" only removes
// 👀.
func (r *Reviewer) reactPickedUp(ctx context.Context) {
	st := reactFrom(ctx)
	if st == nil || !r.reactsTo(st.job) {
		return
	}
	st.mu.Lock()
	st.note, st.quiet = true, true
	st.mu.Unlock()
	r.rx.track(st.job.NoteID, r.env.Now())
	r.reactSubmit(ctx, st, reactions.EventPickedUp, noteTarget(st.job), r.emojis(reactions.EventPickedUp), nil)
}

// reactOnce puts the award of ev (thanks, ack) on the job's note. It
// reports whether a change was queued.
func (r *Reviewer) reactOnce(ctx context.Context, job core.Job, ev reactions.Event) bool {
	if !r.reactsTo(job) {
		return false
	}
	add := r.emojis(ev)
	if len(add) == 0 {
		return false
	}
	r.reactSubmit(ctx, reactFrom(ctx), ev, noteTarget(job), add, nil)
	return true
}

// reactReview puts 🎉 on the MR after a clean review and takes the bot's
// 🎉 away after one that is not.
func (r *Reviewer) reactReview(ctx context.Context, job core.Job, eff config.Effective, clean bool) {
	if r.rx == nil {
		return
	}
	name := eff.Reactions.Emoji(string(reactions.EventCleanReview))
	if name == "" {
		return
	}
	t := reactions.Target{ProjectID: job.ProjectID, MRIID: job.MRIID}
	if clean {
		r.reactSubmit(ctx, reactFrom(ctx), reactions.EventCleanReview, t, []string{name}, nil)
		return
	}
	r.reactSubmit(ctx, reactFrom(ctx), reactions.EventCleanReview, t, nil, []string{name})
}

// reviewClean: nothing new was reported (findings, lint, breaking
// changes), no bot finding or lint thread is still open, nothing blocks
// the merge and the whole diff was reviewed.
func reviewClean(published, outside []output.Finding, blockingOpen int, threads *mrthread.Threads, lr *linters.Run, cr *compatcheck.Run, partial bool) bool {
	if len(published)+len(outside) > 0 || blockingOpen > 0 || partial {
		return false
	}
	if threads != nil && len(threads.Open) > 0 {
		return false
	}
	if lr != nil && len(lr.Published)+len(lr.Outside)+len(lr.Remaining) > 0 {
		return false
	}
	return cr == nil || len(cr.Published)+len(cr.Outside) == 0
}

// reactAllowedFn gates a change on the repository's reactions.enabled.
func (r *Reviewer) reactAllowedFn(projectID, mrIID int64) func(context.Context) bool {
	return func(ctx context.Context) bool { return r.reactAllowed(ctx, projectID, mrIID) }
}

// reactAllowed reports the repository's switch for the MR: cached from the
// job's own effectiveConfig, else read now (GetMR + .reviewbot.yaml). When
// the config cannot be read, the service setting applies.
func (r *Reviewer) reactAllowed(ctx context.Context, projectID, mrIID int64) bool {
	if r.rx == nil || !r.reactCfg().Enabled {
		return false
	}
	key := core.MRKey(projectID, mrIID)
	if on, ok := r.rx.repo(key, r.env.Now()); ok {
		return on
	}
	mr, err := r.env.GitLab.GetMR(ctx, projectID, mrIID)
	if err != nil {
		r.env.Log.Debug("reactions: repo config: get mr", "mr", key, "err", err)
		return true
	}
	eff, err := r.effectiveConfig(ctx, projectID, mr) // fills the cache
	if err != nil {
		r.env.Log.Debug("reactions: repo config", "mr", key, "err", err)
		return true
	}
	return eff.Reactions.Enabled
}
