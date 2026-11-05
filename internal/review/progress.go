package review

// The "review not finished" thread (review.progress_thread): one thread per
// MR, opened the moment an automatic review is queued (before the debounce
// and the queue), deleted when there is nothing left to wait for: the review
// finished, or none is needed (a draft, a pause, a reviewed head ...). It
// stays, with the reason, when the review failed, was postponed or did not
// start (a merge conflict, the limits). It is not blocking by itself (no
// marker.Blocking): it holds the merge only in a project that requires all
// threads to be resolved. The bot never closes it, so a closed thread was
// closed by a person: a deliberate override, the bot neither touches nor
// recreates it. Failures of the thread never fail the review.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

const (
	msgProgressStart   = "⏳ **Идёт ревью.** Тред закроется сам, когда ревью завершится."
	msgProgressFailed  = "⚠️ Ревью не удалось: %s. Повторить: `%s review`"
	msgProgressHeld    = "⛔ Ревью не запущено: %s"
	msgProgressConflct = "⛔ Ревью не запускалось: конфликт слияния с `%s`"
	prefixPostponed    = "⏳ Ревью отложено до "

	progressEarlyTimeout = 30 * time.Second
)

// progressState serializes the work on one MR's thread (the early creation
// runs beside the review job) and remembers the newest queued head.
type progressState struct {
	mu    sync.Mutex
	locks map[string]*progressLock
	want  map[string]string // MR key -> head of the newest queued automatic review
	wg    sync.WaitGroup
}

type progressLock struct {
	mu sync.Mutex
	n  int
}

// lock takes the MR's lock; the returned func releases it.
func (s *progressState) lock(key string) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks, s.want = map[string]*progressLock{}, map[string]string{}
	}
	l := s.locks[key]
	if l == nil {
		l = &progressLock{}
		s.locks[key] = l
	}
	l.n++
	s.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.mu.Lock()
		if l.n--; l.n == 0 {
			delete(s.locks, key)
		}
		s.mu.Unlock()
	}
}

// WaitProgress waits for the threads being opened in the background
// (progressQueued); for tests and a clean shutdown.
func (r *Reviewer) WaitProgress() { r.progress.wg.Wait() }

// progressThread is the bot's progress thread found in the MR.
type progressThread struct {
	id       string
	resolved bool   // closed by a person: the bot never closes it
	last     string // the bot's newest note
	noteIDs  []int64
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
				th.noteIDs = append(th.noteIDs, n.ID)
			}
		}
		found = th
	}
	return found
}

// progressCreate opens the thread with body as its first note.
func (r *Reviewer) progressCreate(ctx context.Context, job core.Job, body string) {
	text := mdtext.NeutralizeQuickActions(marker.ProgressThread + "\n" + body)
	if err := r.env.GitLab.CreateDiscussion(ctx, job.ProjectID, job.MRIID, text); err != nil {
		r.env.Log.Warn("review: progress thread not posted", "mr", job.MRKey(), "err", err)
	}
}

// Accepted's hook: an automatic review was queued, so open the thread now,
// without delaying the queue.
func (r *Reviewer) progressQueued(job core.Job) {
	if !r.env.Cfg.Review.ProgressThread || job.Kind != core.JobAutoReview {
		return
	}
	r.progress.wg.Add(1)
	go func() {
		defer r.progress.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), progressEarlyTimeout)
		defer cancel()
		r.progressEarly(ctx, job)
	}()
}

// progressEarly opens the MR's thread when a review is going to run, judging
// by what the event and one MR read tell (the repository config is not read:
// a review that turns out not to be needed deletes the thread).
func (r *Reviewer) progressEarly(ctx context.Context, job core.Job) {
	log := r.env.Log.With("mr", job.MRKey())
	st, err := r.env.Store.GetMRState(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: progress thread: get mr state", "err", err)
		return
	}
	if !job.ForceFull && st != nil && st.ReviewedSHA != "" && st.ReviewedSHA == job.HeadSHA {
		return // the head is reviewed already
	}
	mr, err := r.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: progress thread: get mr", "err", err)
		return
	}
	if mr.State != "opened" || mr.Draft {
		return
	}
	if skip, err := r.skipAuto(ctx, job, mr); skip || err != nil {
		return // ignore marker or pause
	}
	key := job.MRKey()
	defer r.progress.lock(key)()
	r.progress.want[key] = job.HeadSHA
	if r.findProgressThread(ctx, job) == nil {
		r.progressCreate(ctx, job, msgProgressStart)
	}
}

// progressClear deletes the thread: there is nothing left to wait for. A
// thread closed by a person stays. A review of a newer head is queued
// meanwhile: its thread is this one, so it stays.
func (r *Reviewer) progressClear(ctx context.Context, rv *reviewRun) {
	job := rv.job
	key := job.MRKey()
	defer r.progress.lock(key)()
	head := cmpOr(job.HeadSHA, rv.head)
	if w := r.progress.want[key]; w != "" && head != "" && w != head {
		return
	}
	delete(r.progress.want, key)
	th := r.findProgressThread(ctx, job)
	if th == nil || th.resolved {
		return
	}
	// The first note goes last: a thread without notes is gone.
	for i := len(th.noteIDs) - 1; i >= 0; i-- {
		if err := r.env.GitLab.DeleteNote(ctx, job.ProjectID, job.MRIID, th.noteIDs[i]); err != nil {
			r.env.Log.Warn("review: delete progress thread", "mr", key, "err", err)
			return
		}
	}
}

// progressLeave keeps the thread open and says why the review did not run
// or finish (a new thread starts with that text). quiet: the state did not
// change, so no reply is needed (a retry of the same job).
func (r *Reviewer) progressLeave(ctx context.Context, job core.Job, msg string, quiet func(last string) bool) {
	defer r.progress.lock(job.MRKey())()
	th := r.findProgressThread(ctx, job)
	if th == nil {
		r.progressCreate(ctx, job, msg)
		return
	}
	if th.resolved || quiet(th.last) {
		return
	}
	if err := r.env.GitLab.ReplyToDiscussion(ctx, job.ProjectID, job.MRIID, th.id, mdtext.NeutralizeQuickActions(msg)); err != nil {
		r.env.Log.Warn("review: reply in progress thread", "mr", job.MRKey(), "err", err)
	}
}

// progressFinish records how the review ended (deferred in handleReview,
// whatever the exit): err is handleReview's result.
func (r *Reviewer) progressFinish(ctx context.Context, rv *reviewRun, err error) {
	if ctx.Err() != nil {
		return // shutting down: the job runs again
	}
	same := func(body string) func(string) bool {
		return func(last string) bool { return strings.HasSuffix(last, body) }
	}
	var rl *core.RetryLater
	switch {
	case rv.conflicted:
		msg := fmt.Sprintf(msgProgressConflct, rv.mr.TargetBranch)
		r.progressLeave(ctx, rv.job, msg, same(msg))
	case err == nil && rv.held != "":
		msg := fmt.Sprintf(msgProgressHeld, rv.held)
		r.progressLeave(ctx, rv.job, msg, same(msg))
	case err == nil:
		r.progressClear(ctx, rv)
	case asRetryLater(err, &rl):
		reason := strings.TrimSuffix(cmpOr(rv.held, rl.Reason), ".")
		msg := fmt.Sprintf(msgPostponed, r.env.Clock(rl.At), reason)
		// The time moves on every retry: the same reason is the same state.
		r.progressLeave(ctx, rv.job, msg, func(last string) bool {
			return strings.HasPrefix(last, prefixPostponed) && strings.HasSuffix(last, ": "+reason)
		})
	default:
		msg := fmt.Sprintf(msgProgressFailed, failReason(ctx, err), r.env.Mention())
		r.progressLeave(ctx, rv.job, msg, same(msg))
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
