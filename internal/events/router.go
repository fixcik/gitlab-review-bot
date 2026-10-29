// Package events turns GitLab events (merge request, note and pipeline
// events in neutral form) into core.Job values for the queue. Both event
// sources use the same Router, so they behave the same: the webhook handler
// (internal/webhook) decodes the hook payloads into these events, the poller
// (internal/poller) builds them from API listings. The poller's persisted
// state (PollMR, PollState) is defined here too, so the store implements it
// without importing the poller.
package events

import (
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Options configures a Router.
type Options struct {
	// BotUsername is the bot's GitLab username (without "@"); notes authored
	// by it are ignored.
	BotUsername string
	// Mention is the trigger, e.g. "@reviewbot". Default "@" + BotUsername.
	Mention string
	// Now is used for Job.EnqueuedAt; defaults to time.Now.
	Now func() time.Time
	// Groups, when non-empty, restricts accepted events to projects whose
	// path_with_namespace is one of these groups or lies under one of them
	// (case-insensitive). Empty accepts every project.
	Groups []string
	// LearnReplies enqueues core.JobThreadReply for replies in discussions
	// that do not mention the bot (learning team rules, see thread.go).
	LearnReplies bool
	// ReactThanks / ReactAck: notes that only thank or acknowledge
	// (reactions.Classify) get an award emoji instead of a Claude run:
	// addressed to the bot they become core.JobReact; as plain replies in a
	// thread, core.JobThreadReply even when LearnReplies is off (react.go).
	ReactThanks bool
	ReactAck    bool
	// Excluded, when set, drops events of projects the service excludes
	// from review (gitlab.exclude).
	Excluded func(projectPath string) bool
	// Pipelines accepts pipeline events (finished pipelines become
	// core.JobPipeline, review.ci; pipeline.go). MainBranch names a
	// project's main branch (gitlab.MainBranchFor; nil = the default
	// branch of the event).
	Pipelines  bool
	MainBranch func(projectPath, defaultBranch string) string
}

// Router classifies MR, note and pipeline events into jobs (drafts, Draft →
// Ready, title-only updates, commands, thread replies, the group filter).
// It is safe for concurrent use.
type Router struct {
	opts    Options
	mention string
	now     func() time.Time
	groups  []string // lowercased, trimmed of slashes
}

// NewRouter builds a Router.
func NewRouter(o Options) *Router {
	r := &Router{opts: o, now: o.Now}
	if r.now == nil {
		r.now = time.Now
	}
	r.mention = strings.TrimSpace(o.Mention)
	if r.mention == "" && o.BotUsername != "" {
		r.mention = "@" + strings.TrimPrefix(o.BotUsername, "@")
	}
	for _, g := range o.Groups {
		g = strings.ToLower(strings.Trim(strings.TrimSpace(g), "/"))
		if g != "" {
			r.groups = append(r.groups, g)
		}
	}
	return r
}

// Accepts reports whether events of a project are handled at all (groups
// and gitlab.exclude), so pollers can skip excluded projects early.
func (r *Router) Accepts(projectPath string) bool {
	ok, _ := r.accepts(projectPath)
	return ok
}

// accepts reports whether events of the project are handled: inside the
// configured groups and not excluded.
func (r *Router) accepts(path string) (bool, string) {
	if !r.inGroups(path) {
		return false, "project " + path + " is outside the configured groups"
	}
	if r.opts.Excluded != nil && r.opts.Excluded(path) {
		return false, "project " + path + " is excluded (gitlab.exclude)"
	}
	return true, ""
}

// inGroups reports whether the project path belongs to one of the configured
// groups (or no groups are configured).
func (r *Router) inGroups(path string) bool {
	if len(r.groups) == 0 {
		return true
	}
	p := strings.ToLower(strings.Trim(strings.TrimSpace(path), "/"))
	if p == "" {
		return false
	}
	for _, g := range r.groups {
		if p == g || strings.HasPrefix(p, g+"/") {
			return true
		}
	}
	return false
}

// finish applies the project filter (groups, gitlab.exclude) to a job and
// stamps it; every event kind goes through it.
func (r *Router) finish(job *core.Job, why string) (*core.Job, string) {
	if job == nil {
		return nil, why
	}
	if ok, reason := r.accepts(job.ProjectPath); !ok {
		return nil, reason
	}
	job.EnqueuedAt = r.now()
	return job, why
}

// MREvent is a merge request event (Merge Request Hook) in neutral form.
type MREvent struct {
	ProjectID   int64
	ProjectPath string
	IID         int64
	State       string // opened, closed, merged, locked
	// Draft: the MR is a draft (or work in progress).
	Draft bool
	// Action is "open", "reopen" or "update".
	Action  string
	HeadSHA string
	// OldRev is the previous head when the update brought new commits.
	OldRev string
	// BecameReady: the MR changed from Draft to Ready in this update.
	BecameReady bool
	// OldTitle/NewTitle are set when the title changed in this update.
	OldTitle, NewTitle string
}

// MR returns the job for e, or nil and the reason it is ignored.
func (r *Router) MR(e MREvent) (*core.Job, string) {
	return r.finish(mrJob(e))
}

func mrJob(e MREvent) (*core.Job, string) {
	if e.ProjectID == 0 || e.IID == 0 {
		return nil, "missing project or iid"
	}
	if e.State != "opened" {
		return nil, "state " + e.State
	}
	if e.Draft {
		return nil, "draft"
	}
	job := &core.Job{
		Kind:        core.JobAutoReview,
		ProjectID:   e.ProjectID,
		ProjectPath: e.ProjectPath,
		MRIID:       e.IID,
		HeadSHA:     e.HeadSHA,
	}
	switch e.Action {
	case "open", "reopen":
		return job, ""
	case "update":
		if e.BecameReady {
			job.ForceFull = true
			return job, ""
		}
		if e.OldRev != "" {
			return job, ""
		}
		if e.OldTitle != e.NewTitle {
			// Only the title changed: refresh the title hint in the summary
			// without a review.
			job.Kind = core.JobRefreshTitle
			job.HeadSHA = ""
			return job, ""
		}
		return nil, "update without new commits"
	default:
		return nil, "action " + e.Action
	}
}
