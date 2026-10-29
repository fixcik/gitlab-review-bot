package review

// Notes that only thank (❤️) or acknowledge (👍) the bot get an award emoji
// instead of a Claude run: core.JobReact ("@bot спасибо", "@bot ок") and
// plain replies in bot threads (learning.NoteReactions.ThreadReply).

import (
	"context"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/reactions"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

// handleReact answers a note that only thanks (❤️) or acknowledges (👍)
// the bot with an award emoji: no Claude run, no reply, no limits. It
// falls back to the chat flow when reactions are off for the repository
// (or for this user), and for an acknowledgement outside a bot remark
// thread.
func (r *Reviewer) handleReact(ctx context.Context, job core.Job) error {
	log := r.env.Log.With("mr", job.MRKey(), "kind", job.Kind, "user", job.Username, "note", job.NoteID)
	var ev reactions.Event
	switch reactions.Classify(job.Question, r.env.Mention()) {
	case reactions.Thanks:
		ev = reactions.EventThanks
	case reactions.Ack:
		ev = reactions.EventAck
	}
	if ev == "" || len(r.emojis(ev)) == 0 || !r.reactsTo(job) || !r.reactAllowed(ctx, job.ProjectID, job.MRIID) {
		return r.reactFallback(ctx, job, "reactions off")
	}
	if ev == reactions.EventAck && !r.inBotRemarkThread(ctx, job) {
		return r.reactFallback(ctx, job, "acknowledgement outside a bot remark thread")
	}
	r.reactOnce(ctx, job, ev)
	log.Info("reactions: answered with an award emoji, no claude run", "event", ev)
	return nil
}

// reactFallback handles a JobReact note as the chat question it was.
func (r *Reviewer) reactFallback(ctx context.Context, job core.Job, why string) error {
	r.env.Log.Info("reactions: handled as a chat question", "mr", job.MRKey(), "note", job.NoteID, "why", why)
	job.Kind = core.JobChat
	jobctx.AttributeKind(ctx, meter.KindForJob(core.JobChat)) // billed as a question on /usage, not "other"
	if st := reactFrom(ctx); st != nil {
		st.mu.Lock()
		st.job = job
		st.mu.Unlock()
		r.reactLifecycle(ctx, st) // 👀 … ✅ like any question
	}
	return r.handleChat(ctx, job)
}

// threadQuestion answers a reply in a bot thread that learning recognized as
// a question to the bot: the job becomes a chat question (billed as one on
// /usage), and its note gets the usual 👀 … ↩️ instead of the quiet 👀 of a
// learning job.
func (r *Reviewer) threadQuestion(ctx context.Context, job core.Job) error {
	job.Kind = core.JobChat
	jobctx.AttributeKind(ctx, meter.KindForJob(core.JobChat))
	if st := reactFrom(ctx); st != nil {
		st.mu.Lock()
		st.job, st.quiet = job, false
		st.mu.Unlock()
		r.reactLifecycle(ctx, st)
	}
	return r.handleChat(ctx, job)
}

// inBotRemarkThread reports whether the job's note answers a bot remark
// (finding, breaking change, lint thread).
func (r *Reviewer) inBotRemarkThread(ctx context.Context, job core.Job) bool {
	if job.DiscussionID == "" {
		return false
	}
	disc, err := r.env.GitLab.GetDiscussion(ctx, job.ProjectID, job.MRIID, job.DiscussionID)
	if err != nil || disc == nil || len(disc.Notes) == 0 {
		return false
	}
	return r.isBotRemark(disc.Notes[0])
}

// isBotRemark: a bot note that starts a finding, breaking-change or lint
// thread.
func (r *Reviewer) isBotRemark(n core.Note) bool {
	if n.System || !r.env.IsBotNote(n) {
		return false
	}
	if _, ok := marker.ParseFinding(n.Body); ok {
		return true
	}
	if _, _, ok := marker.ParseCompat(n.Body); ok {
		return true
	}
	_, _, ok := marker.ParseLint(n.Body)
	return ok
}

// reactThreadReply handles a plain reply in a bot thread that only thanks
// or acknowledges: ❤️ for thanks right after a bot note, 👍 for an
// acknowledgement (or a fix claim: the next review checks it) in a bot
// remark thread. It reports whether the reply was such a note (then there
// is nothing to learn from it either).
func (r *Reviewer) reactThreadReply(ctx context.Context, job core.Job, disc *core.Discussion, idx int) bool {
	note := disc.Notes[idx]
	switch reactions.Classify(note.Body, r.env.Mention()) {
	case reactions.Thanks:
		prev := -1
		for i := idx - 1; i >= 0; i-- {
			if !disc.Notes[i].System {
				prev = i
				break
			}
		}
		// Thanks to the bot, not to a colleague who answered in between.
		if prev >= 0 && r.env.IsBotNote(disc.Notes[prev]) {
			r.reactOnce(ctx, job, reactions.EventThanks)
		}
		return true
	case reactions.Ack, reactions.Fixed:
		if r.isBotRemark(disc.Notes[0]) {
			r.reactOnce(ctx, job, reactions.EventAck)
		}
		return true
	}
	return false
}
