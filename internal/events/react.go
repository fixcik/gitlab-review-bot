package events

import (
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/reactions"
)

// Reactions (internal/reactions, docs/features/reactions.md).
//
// A note addressed to the bot that only thanks ("@bot спасибо!") or only
// acknowledges ("@bot ок, поправлю") becomes core.JobReact: the reviewer
// puts ❤️ / 👍 on it without a Claude run (falling back to chat when the
// repository turned reactions off, or for an acknowledgement outside a bot
// remark thread). A fix claim ("@bot исправил") stays a chat question: the
// chat run re-checks the code and may resolve the thread.
//
// A plain reply without the mention in a threaded discussion that only
// thanks or acknowledges becomes core.JobThreadReply even when learning is
// off, so the reviewer can react to it once it has checked the thread is
// the bot's.

// routeReact turns a chat question into core.JobReact when the whole note
// (not only the text after the mention: "почему? @bot спасибо" is a
// question) is a pure thanks / ack.
func (r *Router) routeReact(kind core.JobKind, note string) core.JobKind {
	if kind != core.JobChat {
		return kind
	}
	switch reactions.Classify(note, r.mention) {
	case reactions.Thanks:
		if r.opts.ReactThanks {
			return core.JobReact
		}
	case reactions.Ack:
		if r.opts.ReactAck {
			return core.JobReact
		}
	}
	return kind
}

// reactReply reports whether a threaded note without the mention is worth
// a JobThreadReply for reactions alone.
func (r *Router) reactReply(body string) bool {
	switch reactions.Classify(body, r.mention) {
	case reactions.Thanks:
		return r.opts.ReactThanks
	case reactions.Ack, reactions.Fixed:
		return r.opts.ReactAck
	}
	return false
}
