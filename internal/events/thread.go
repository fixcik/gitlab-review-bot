package events

import (
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
)

// Thread replies (learning team rules).
//
// GitLab's Note Hook does not say whether a note is the first one of its
// discussion. What the payload does give is object_attributes.type:
// "DiscussionNote" (a note in a threaded discussion) or "DiffNote" (a note in
// a diff thread), versus null for a plain, non-threaded comment
// (NoteEvent.Threaded). We enqueue core.JobThreadReply for every threaded
// note (either type, with a discussion_id) by a person that does not mention
// the bot. The handler then
// reads the discussion and goes on only when the note is NOT its first note
// and the first note was written by the bot; everything else (a human
// starting a new thread, a plain comment) ends there without a Claude run.
// The bot's own notes and system notes are dropped before this point.

// replyJob builds the JobThreadReply for a threaded note without a mention.
func (r *Router) replyJob(e NoteEvent) (*core.Job, string) {
	if !e.Threaded || e.DiscussionID == "" || (!r.opts.LearnReplies && !r.reactReply(e.Body)) {
		return nil, "no mention"
	}
	if e.ProjectID == 0 || e.IID == 0 || e.NoteID == 0 {
		return nil, "missing project, iid or note id"
	}
	return &core.Job{
		Kind:         core.JobThreadReply,
		ProjectID:    e.ProjectID,
		ProjectPath:  e.ProjectPath,
		MRIID:        e.IID,
		UserID:       e.UserID,
		Username:     e.Username,
		DiscussionID: e.DiscussionID,
		NoteID:       e.NoteID,
	}, ""
}

// routeRules turns a chat question that is a rule command ("запомни …",
// "забудь #12", "правила", "правило #12 для всех") into core.JobRules.
func routeRules(kind core.JobKind, question string) core.JobKind {
	if kind == core.JobChat && learn.IsCommand(question) {
		return core.JobRules
	}
	return kind
}
