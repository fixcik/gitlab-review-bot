package review

// The "review not finished" thread (review.progress_thread): one thread per
// MR, opened when a review is about to run, closed when it is done. It is not
// blocking by itself (no marker.Blocking): it holds the merge only in a
// project that requires all threads to be resolved. A person who closes it
// overrides it: the bot does not reopen it. Failures of the thread never
// fail the review.

import (
	"context"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
)

const (
	msgProgressStart   = "⏳ **Идёт ревью.** Тред закроется сам, когда ревью завершится."
	msgProgressCommit  = "⏳ Ревью нового коммита `%s`…"
	msgProgressDone    = "✅ Ревью завершено (`%s`)"
	msgProgressFailed  = "⚠️ Ревью не удалось: %s. Повторить: `%s review`"
	msgProgressHeld    = "⛔ Ревью не запущено: %s"
	msgProgressConflct = "⛔ Ревью не запускалось: конфликт слияния с `%s`"
	prefixPostponed    = "⏳ Ревью отложено до "
)

// progressThread is the bot's progress thread found in the MR.
type progressThread struct {
	id       string
	resolved bool
	byHuman  bool   // a person resolved it
	last     string // the bot's newest note
}

// findProgressThread returns the newest progress thread of the bot (nil when
// none or unreadable). It reads GitLab fresh: the job's memo is stale after
// this job's own writes.
func (r *Reviewer) findProgressThread(ctx context.Context, job core.Job) *progressThread {
	ds, err := r.env.GitLab.ListDiscussions(jobctx.WithFreshGitLab(ctx), job.ProjectID, job.MRIID)
	if err != nil {
		r.env.Log.Warn("review: list discussions for the progress thread", "mr", job.MRKey(), "err", err)
		return nil
	}
	var found *progressThread
	for _, d := range ds {
		if len(d.Notes) == 0 {
			continue
		}
		first := d.Notes[0]
		if first.System || !r.env.IsBotNote(first) || !strings.Contains(marker.Header(first.Body), marker.ProgressThread) {
			continue
		}
		th := &progressThread{id: d.ID, resolved: d.Resolved}
		for _, n := range d.Notes {
			if !n.System && r.env.IsBotNote(n) {
				th.last = n.Body
			}
			if n.ResolvedByID != 0 || n.ResolvedByUsername != "" {
				th.byHuman = !r.env.IsBotNote(core.Note{AuthorID: n.ResolvedByID, AuthorUsername: n.ResolvedByUsername})
			}
		}
		found = th
	}
	return found
}

// progressUpdate is one change of the thread.
type progressUpdate struct {
	create  string                        // the first note's text when there is no thread ("" = do not create)
	reply   string                        // the reply to an existing thread
	quiet   func(th *progressThread) bool // no reply is needed (the state did not change)
	resolve bool                          // close the thread, otherwise reopen it
}

// progressApply applies u to the MR's progress thread. Errors are logged:
// the indicator never fails the review.
func (r *Reviewer) progressApply(ctx context.Context, job core.Job, u progressUpdate) {
	if !r.env.Cfg.Review.ProgressThread {
		return
	}
	log := r.env.Log.With("mr", job.MRKey())
	th := r.findProgressThread(ctx, job)
	if th == nil {
		if u.create == "" {
			return
		}
		body := marker.ProgressThread + "\n" + u.create
		if created, err := r.pub.Drafts(ctx, job, []publish.Draft{{Body: mdtext.NeutralizeQuickActions(body), What: "progress thread"}}); err != nil || !created[0] {
			log.Warn("review: progress thread not posted", "err", err)
			return
		}
		if th = r.findProgressThread(ctx, job); th == nil || !u.resolve {
			return
		}
		u.reply, u.quiet = "", nil
	}
	if th.resolved && th.byHuman {
		return // a person closed it: a deliberate override
	}
	gl := r.env.GitLab
	if !u.resolve && th.resolved {
		if err := gl.ResolveDiscussion(ctx, job.ProjectID, job.MRIID, th.id, false); err != nil {
			log.Warn("review: reopen progress thread", "err", err)
			return
		}
	}
	if u.reply != "" && (u.quiet == nil || !u.quiet(th)) {
		if err := gl.ReplyToDiscussion(ctx, job.ProjectID, job.MRIID, th.id, mdtext.NeutralizeQuickActions(u.reply)); err != nil {
			log.Warn("review: reply in progress thread", "err", err)
		}
	}
	if u.resolve && !th.resolved {
		if err := r.env.Resolve(ctx, job.ProjectID, job.MRIID, th.id); err != nil {
			log.Warn("review: resolve progress thread", "err", err)
		}
	}
}

// progressStart opens the thread (or reopens it for a new commit) at the
// start of a review that will run.
func (r *Reviewer) progressStart(ctx context.Context, rv *reviewRun) {
	r.progressApply(ctx, rv.job, progressUpdate{
		create: msgProgressStart,
		reply:  fmt.Sprintf(msgProgressCommit, mdtext.ShortSHA(rv.head)),
		// A thread that is open and shows a failure, a postponement or a
		// running review is the same review (a retry): no new reply.
		quiet: func(th *progressThread) bool { return !th.resolved && !strings.HasPrefix(th.last, "✅") },
	})
}

// progressConflict leaves an open thread that says why the review did not run.
func (r *Reviewer) progressConflict(ctx context.Context, rv *reviewRun) {
	msg := fmt.Sprintf(msgProgressConflct, rv.mr.TargetBranch)
	r.progressApply(ctx, rv.job, progressUpdate{create: msg, reply: msg,
		quiet: func(th *progressThread) bool { return strings.HasSuffix(th.last, msg) }})
}

// progressFinish records how the review ended (a deferred call after
// progressStart): err is handleReview's result.
func (r *Reviewer) progressFinish(ctx context.Context, rv *reviewRun, err error) {
	if ctx.Err() != nil {
		return // shutting down: the job runs again
	}
	same := func(body string) func(*progressThread) bool {
		return func(th *progressThread) bool { return strings.HasSuffix(th.last, body) }
	}
	var rl *core.RetryLater
	switch {
	case err == nil && rv.held != "":
		msg := fmt.Sprintf(msgProgressHeld, rv.held)
		r.progressApply(ctx, rv.job, progressUpdate{reply: msg, quiet: same(msg)})
	case err == nil:
		msg := fmt.Sprintf(msgProgressDone, mdtext.ShortSHA(rv.head))
		r.progressApply(ctx, rv.job, progressUpdate{reply: msg, quiet: same(msg), resolve: true})
	case asRetryLater(err, &rl):
		reason := strings.TrimSuffix(cmpOr(rv.held, rl.Reason), ".")
		msg := fmt.Sprintf(msgPostponed, r.env.Clock(rl.At), reason)
		// The time moves on every retry: the same reason is the same state.
		r.progressApply(ctx, rv.job, progressUpdate{reply: msg, quiet: func(th *progressThread) bool {
			return strings.HasPrefix(th.last, prefixPostponed) && strings.HasSuffix(th.last, ": "+reason)
		}})
	default:
		msg := fmt.Sprintf(msgProgressFailed, failReason(ctx, err), r.env.Mention())
		r.progressApply(ctx, rv.job, progressUpdate{reply: msg, quiet: same(msg)})
	}
}

func asRetryLater(err error, rl **core.RetryLater) bool {
	v, ok := core.AsRetryLater(err)
	*rl = v
	return ok
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
