package env

import (
	"context"
	"fmt"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

// MsgNoAccess is the reply to a command of a user below Developer.
const MsgNoAccess = "Недостаточно прав: команды бота доступны участникам проекта с ролью Developer и выше."

// Reply answers a command: in its discussion, or as a top-level note. It
// is the job's final reply (handlers return right after it): once GitLab
// accepted it, the running queue job is marked answered
// (core.RunState.MarkAnswered), so a restart that interrupts the job from
// now on drops it instead of replaying it and answering twice. Other bot
// notes of a job (a "fixed" reply in a finding thread, the summary) never
// mark it. A reply that announces a postponement is followed by a
// *core.RetryLater, which resets the mark (queue retryLocked); a crash in
// between drops that job.
func (e *Env) Reply(ctx context.Context, job core.Job, body string) error {
	body = mdtext.NeutralizeQuickActions(body)
	if job.DiscussionID != "" {
		if err := e.GitLab.ReplyToDiscussion(ctx, job.ProjectID, job.MRIID, job.DiscussionID, body); err != nil {
			return fmt.Errorf("reply to discussion: %w", err)
		}
		markAnswered(ctx, job)
		e.closeCommandThread(ctx, job)
		return nil
	}
	if job.Username != "" {
		body = "@" + job.Username + " " + body
	}
	if _, err := e.GitLab.CreateNote(ctx, job.ProjectID, job.MRIID, body); err != nil {
		return fmt.Errorf("create note: %w", err)
	}
	markAnswered(ctx, job)
	return nil
}

// markAnswered marks the running queue job answered when job is that job
// (same MR and note).
func markAnswered(ctx context.Context, job core.Job) {
	s := core.RunStateFrom(ctx)
	if s == nil {
		return
	}
	if r := s.Job(); r.ProjectID == job.ProjectID && r.MRIID == job.MRIID && r.NoteID == job.NoteID {
		s.MarkAnswered()
	}
}

// CheckAccess replies and returns false when the user is below Developer.
func (e *Env) CheckAccess(ctx context.Context, job core.Job) (bool, error) {
	lvl, err := e.GitLab.AccessLevel(ctx, job.ProjectID, job.UserID)
	if err != nil {
		return false, fmt.Errorf("access level: %w", err)
	}
	if lvl >= core.AccessDeveloper {
		return true, nil
	}
	jobctx.Denied(ctx) // 🚫 next to the reply (review/reactions.go)
	return false, e.Reply(ctx, job, MsgNoAccess)
}

// closeCommandThread resolves the thread a service command started, after
// the answer: GitLab 19.4 turns a plain MR comment into a resolvable thread,
// and an open one blocks the merge under "All threads must be resolved".
// Only with the service token (the bot's Reporter token cannot resolve), only
// for a thread whose first note is the command itself (a command typed as a
// reply inside a finding thread must not close that finding), and best
// effort: a failure is logged, never returned.
func (e *Env) closeCommandThread(ctx context.Context, job core.Job) {
	info, ok := job.Kind.Info()
	if !ok || !info.ClosesThread || e.Mod == nil || job.NoteID == 0 {
		return
	}
	d, err := e.GitLab.GetDiscussion(ctx, job.ProjectID, job.MRIID, job.DiscussionID)
	if err != nil || d == nil || !d.Resolvable || d.Resolved || len(d.Notes) == 0 || d.Notes[0].ID != job.NoteID || e.IsBotNote(d.Notes[0]) {
		if err != nil {
			e.Log.Warn("command thread: read failed", "discussion", job.DiscussionID, "err", err)
		}
		return
	}
	if err := e.Mod.ResolveDiscussion(ctx, job.ProjectID, job.MRIID, job.DiscussionID, true); err != nil {
		e.Log.Warn("command thread: resolve failed", "discussion", job.DiscussionID, "err", err)
	}
}
